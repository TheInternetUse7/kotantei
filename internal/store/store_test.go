package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenCreatesVersionedSQLiteWithBoundedSettings(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "directory with spaces", "kotantei.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	for query, want := range map[string]int{
		"PRAGMA foreign_keys":                        1,
		"PRAGMA busy_timeout":                        5000,
		"PRAGMA cache_size":                          -2048,
		"SELECT MAX(version) FROM schema_migrations": 3,
	} {
		var got int
		if err := s.DB().QueryRowContext(ctx, query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
	var mode string
	if err := s.DB().QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatal(err)
	}
	if mode != "wal" {
		t.Fatalf("journal mode = %s", mode)
	}
	if s.DB().Stats().MaxOpenConnections != 1 {
		t.Fatal("expected a bounded, single-connection store")
	}
}

func TestOpenPreservesDataAcrossReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kotantei.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO users(id) VALUES ('18446744073709551615')`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	summary, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Users != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
}

func TestReplacementSQLiteConnectionsKeepForeignKeysAndCacheBounds(t *testing.T) {
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "replacement.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	// Force database/sql to replace the connection between every operation.
	s.DB().SetMaxIdleConns(0)
	for query, want := range map[string]int{"PRAGMA foreign_keys": 1, "PRAGMA cache_size": -2048, "PRAGMA busy_timeout": 5000} {
		var got int
		if err := s.DB().QueryRow(query).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("replacement connection: %s = %d, want %d", query, got, want)
		}
	}
	if _, err := s.DB().Exec(`INSERT INTO guild_users(guild_id,user_id) VALUES ('missing','missing')`); err == nil {
		t.Fatal("replacement connection stopped enforcing relationships")
	}
}

func TestSchemaEnforcesRelationshipsAndAccountUniqueness(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "kotantei.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO guild_users(guild_id,user_id) VALUES ('1','2')`); err == nil {
		t.Fatal("foreign keys not enforced")
	}
	if _, err := s.DB().ExecContext(ctx, `INSERT INTO users(id) VALUES ('1'),('2'); INSERT INTO accounts(provider,external_id,user_id) VALUES ('anilist','99','1')`); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO accounts(provider,external_id,user_id) VALUES ('anilist','99','2')`,
		`INSERT INTO accounts(provider,external_id,user_id) VALUES ('anilist','100','1')`,
		`INSERT INTO accounts(provider,external_id,user_id) VALUES ('unknown','101','2')`,
		`UPDATE accounts SET colors='{}'`,
	} {
		if _, err := s.DB().ExecContext(ctx, query); err == nil {
			t.Fatalf("invalid relationship/settings accepted: %s", query)
		}
	}
}

func TestOpenRefusesForeignDatabaseWithoutChangingIt(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "tantei.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE AniListUsers(Id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := fileDigest(t, path)
	if s, err := Open(ctx, path); !errors.Is(err, ErrForeignDatabase) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("expected foreign-database error, got %v", err)
	}
	if fileDigest(t, path) != before {
		t.Fatal("foreign database was changed")
	}
}

func TestOpenRefusesForeignSchemaWithoutTables(t *testing.T) {
	for _, setup := range []string{`CREATE VIEW unrelated AS SELECT 1 AS value`, `PRAGMA application_id=42`} {
		t.Run(setup, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "foreign.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(setup); err != nil {
				t.Fatal(err)
			}
			db.Close()
			before := fileDigest(t, path)
			s, err := Open(context.Background(), path)
			if s != nil {
				s.Close()
			}
			if !errors.Is(err, ErrForeignDatabase) {
				t.Fatalf("expected foreign schema rejection, got %v", err)
			}
			if fileDigest(t, path) != before {
				t.Fatal("foreign schema was changed")
			}
		})
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kotantei.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES (999,0)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := fileDigest(t, path)
	if s, err := Open(ctx, path); !errors.Is(err, ErrNewerSchema) {
		if s != nil {
			s.Close()
		}
		t.Fatalf("expected newer-schema error, got %v", err)
	}
	if fileDigest(t, path) != before {
		t.Fatal("newer database was changed")
	}
}

func TestReadOnlyConnectionCannotModifyOrCreateFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.db")
	if db, err := OpenReadOnly(ctx, missing); err == nil {
		db.Close()
		t.Fatal("opened missing database")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("read-only open created a file")
	}
	path := filepath.Join(dir, "present.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := fileDigest(t, path)
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO users(id) VALUES ('1')`); err == nil {
		t.Fatal("read-only connection allowed a write")
	}
	db.Close()
	if fileDigest(t, path) != before {
		t.Fatal("read-only open changed source contents")
	}
}

func TestSummaryIncludesOnlyRecordCounts(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "kotantei.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	_, err = s.DB().Exec(`INSERT INTO users(id) VALUES ('1'),('2');
		INSERT INTO guilds(id,channel_id) VALUES ('10','11');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1'),('10','2');
		INSERT INTO accounts(provider,external_id,user_id) VALUES ('anilist','101','1'),('mal','201','2');
		INSERT INTO favorites(account_id,kind,external_id) SELECT id,'anime','42' FROM accounts WHERE provider='anilist'`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got != (Summary{Users: 2, Guilds: 1, Memberships: 2, Accounts: 2, Favorites: 1}) {
		t.Fatalf("unexpected summary: %+v", got)
	}
}

func TestInspectIsReadOnlyAndRejectsNonNativeStorage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "kotantei.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := fileDigest(t, path)
	if summary, err := Inspect(ctx, path); err != nil || summary != (Summary{}) {
		t.Fatalf("inspection failed: %+v %v", summary, err)
	}
	if fileDigest(t, path) != before {
		t.Fatal("inspection changed the database")
	}
	foreign := filepath.Join(t.TempDir(), "foreign.db")
	db, err := sql.Open("sqlite", foreign)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated(value TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Inspect(ctx, foreign); !errors.Is(err, ErrForeignDatabase) {
		t.Fatalf("expected unknown database error, got %v", err)
	}
}

func fileDigest(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(data)
}
