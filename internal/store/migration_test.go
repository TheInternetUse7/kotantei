package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrationAddsOutboxWithoutLosingVersionOneData(t *testing.T) {
	ctx := context.Background()
	path := versionOneDatabase(t)
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var revision int64
	if err := s.DB().QueryRow(`SELECT revision FROM accounts WHERE id=1`).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if revision != 0 {
		t.Fatalf("initial revision = %d", revision)
	}
	var checkpoint int64
	if err := s.DB().QueryRow(`SELECT value FROM checkpoints WHERE account_id=1 AND kind='activity'`).Scan(&checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint != 1794000000 {
		t.Fatal("migration changed an imported checkpoint")
	}
	for _, table := range []string{"checkpoint_items", "events", "deliveries", "delivery_channels"} {
		var count int
		if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name=?`, table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("missing outbox table %s", table)
		}
	}
	summary, err := s.Summary(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Users != 1 || summary.Accounts != 1 || summary.Favorites != 1 {
		t.Fatalf("lost legacy records: %+v", summary)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var versions int
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 3 {
		t.Fatalf("migration reran on reopen: %d versions", versions)
	}
}

func TestInspectDoesNotMigrateVersionOneStorage(t *testing.T) {
	path := versionOneDatabase(t)
	before := fileDigest(t, path)
	summary, err := Inspect(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Accounts != 1 {
		t.Fatal("inspection lost account count")
	}
	if fileDigest(t, path) != before {
		t.Fatal("read-only inspection migrated the database")
	}
}

func TestInvalidMigrationHistoryIsRejectedWithoutWrites(t *testing.T) {
	path := versionOneDatabase(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE schema_migrations SET version=0`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	before := fileDigest(t, path)
	s, err := Open(context.Background(), path)
	if s != nil {
		s.Close()
	}
	if err == nil {
		t.Fatal("accepted invalid migration history")
	}
	if fileDigest(t, path) != before {
		t.Fatal("invalid database was modified")
	}
}

func TestVersionThreeMigrationPreservesExistingOutboxAndAddsRuntimeGenerations(t *testing.T) {
	path := versionOneDatabase(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(schemaV2 + `INSERT INTO schema_migrations(version,applied_at) VALUES (2,0);
		INSERT INTO checkpoint_items(account_id,kind,source_id) VALUES (1,'activity','observed');
		INSERT INTO events(id,account_id,event_key,stream,kind,source_id,subject_kind,subject_id,occurred_at,payload,created_at) VALUES (1,1,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','activity','list','1','anime','42',1794000001000000,'{}',1794000002000000);
		INSERT INTO deliveries(event_id,channel_id,available_at,created_at) VALUES (1,'11',1794000002000000,1794000002000000);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, count int
	if err := s.DB().QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatal("runtime generation migration was not applied", version)
	}
	if err := s.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE type='table' AND name='runtime_counters'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("durable ID counters missing", err)
	}
	if countRows(t, s, "events") != 1 || countRows(t, s, "deliveries") != 1 || countRows(t, s, "checkpoint_items") != 1 {
		t.Fatal("runtime migration reset pending work or checkpoints")
	}
	var generation int64
	if err := s.DB().QueryRow(`SELECT generation FROM guild_users`).Scan(&generation); err != nil || generation != 0 {
		t.Fatal("imported membership changed", err)
	}
}

func versionOneDatabase(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "version-one.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(schemaV1); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`
		INSERT INTO schema_migrations(version,applied_at) VALUES (1,0);
		INSERT INTO users(id) VALUES ('1');
		INSERT INTO guilds(id,channel_id) VALUES ('10','11');
		INSERT INTO guild_users(guild_id,user_id) VALUES ('10','1');
		INSERT INTO accounts(id,provider,external_id,user_id) VALUES (1,'anilist','101','1');
		INSERT INTO checkpoints(account_id,kind,value) VALUES (1,'activity',1794000000);
		INSERT INTO favorites(account_id,kind,external_id) VALUES (1,'anime','42');
	`); err != nil {
		t.Fatal(err)
	}
	return path
}
