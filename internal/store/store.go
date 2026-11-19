// Package store owns Kotantei's SQLite database and schema migrations.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	ErrForeignDatabase = errors.New("database is not a Kotantei database; use the Tantei importer with a separate destination")
	ErrNewerSchema     = errors.New("database schema is newer than this Kotantei version")
)

type Store struct {
	db   *sql.DB
	path string
	wake chan struct{}
}
type Summary struct {
	Users       int `json:"users"`
	Guilds      int `json:"guilds"`
	Memberships int `json:"memberships"`
	Accounts    int `json:"accounts"`
	Favorites   int `json:"favorites"`
}

const schemaVersion = 3
const applicationID = 0x4b4f544e // KOTN: identify our schema, not arbitrary SQLite files.

func Open(ctx context.Context, path string) (*Store, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0700); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	file, err := os.OpenFile(abs, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err == nil {
		if err := file.Close(); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create database: %w", err)
	}
	db, err := connect(ctx, abs, false)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			db.Close()
		}
	}()
	version, err := inspectSchema(ctx, db)
	if err != nil {
		return nil, err
	}
	for _, pragma := range []string{"PRAGMA foreign_keys=ON", "PRAGMA journal_mode=WAL", "PRAGMA cache_size=-2048"} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			return nil, fmt.Errorf("configure SQLite: %w", err)
		}
	}
	if version < schemaVersion {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return nil, err
		}
		defer tx.Rollback()
		migrations := []string{schemaV1, schemaV2, schemaV3}
		for next := version; next < schemaVersion; next++ {
			if _, err := tx.ExecContext(ctx, migrations[next]); err != nil {
				return nil, fmt.Errorf("apply schema migration %d: %w", next+1, err)
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version,applied_at) VALUES (?,?)`, next+1, time.Now().UTC().Unix()); err != nil {
				return nil, err
			}
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	ok = true
	return &Store{db: db, path: abs, wake: make(chan struct{}, 1)}, nil
}

// OpenReadOnly is also used for foreign legacy schemas. Do not add
// immutable=1: that would ignore committed data still residing in WAL.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	abs, err := absolutePath(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, fmt.Errorf("open existing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("database path must refer to a regular file")
	}
	return connect(ctx, abs, true)
}

func absolutePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("database path must not be empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

func connect(ctx context.Context, path string, readOnly bool) (*sql.DB, error) {
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" && !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	uri := url.URL{Scheme: "file", Path: uriPath}
	query := url.Values{}
	query.Add("_pragma", "busy_timeout(5000)")
	if readOnly {
		query.Set("mode", "ro")
		query.Add("_pragma", "query_only(1)")
	} else {
		query.Set("mode", "rw")
		query.Add("_pragma", "foreign_keys(1)")
		query.Add("_pragma", "cache_size(-2048)")
	}
	uri.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", uri.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	return db, nil
}

func inspectSchema(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT name,type FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return 0, fmt.Errorf("read SQLite schema: %w", err)
	}
	tables := map[string]bool{}
	for rows.Next() {
		var name, kind string
		if err := rows.Scan(&name, &kind); err != nil {
			rows.Close()
			return 0, err
		}
		tables[name] = kind == "table"
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var id, version, count, first int
	if err := db.QueryRowContext(ctx, `PRAGMA application_id`).Scan(&id); err != nil {
		return 0, err
	}
	if len(tables) == 0 && id == 0 {
		return 0, nil
	}
	if !tables["schema_migrations"] {
		return 0, ErrForeignDatabase
	}
	if id != applicationID {
		return 0, ErrForeignDatabase
	}
	if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0),COUNT(*),COALESCE(MIN(version),0) FROM schema_migrations`).Scan(&version, &count, &first); err != nil {
		return 0, fmt.Errorf("invalid schema version metadata: %w", err)
	}
	if version > schemaVersion {
		return 0, ErrNewerSchema
	}
	if version < 1 || first != 1 || count != version {
		return 0, ErrForeignDatabase
	}
	for _, table := range []string{"users", "guilds", "guild_users", "accounts", "checkpoints", "favorites", "imports"} {
		if !tables[table] {
			return 0, ErrForeignDatabase
		}
	}
	if version >= 2 {
		for _, table := range []string{"checkpoint_items", "events", "deliveries", "delivery_channels"} {
			if !tables[table] {
				return 0, ErrForeignDatabase
			}
		}
	}
	if version >= 3 && !tables["runtime_counters"] {
		return 0, ErrForeignDatabase
	}
	return version, nil
}
func (s *Store) DB() *sql.DB                                  { return s.db }
func (s *Store) Path() string                                 { return s.path }
func (s *Store) Close() error                                 { return s.db.Close() }
func (s *Store) Summary(ctx context.Context) (Summary, error) { return summarize(ctx, s.db) }

func Inspect(ctx context.Context, path string) (Summary, error) {
	db, err := OpenReadOnly(ctx, path)
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	version, err := inspectSchema(ctx, db)
	if err != nil {
		return Summary{}, err
	}
	if version == 0 {
		return Summary{}, ErrForeignDatabase
	}
	return summarize(ctx, db)
}

func summarize(ctx context.Context, db sqlReader) (Summary, error) {
	var summary Summary
	err := db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM users), (SELECT COUNT(*) FROM guilds),
		(SELECT COUNT(*) FROM guild_users), (SELECT COUNT(*) FROM accounts),
		(SELECT COUNT(*) FROM favorites)`).Scan(&summary.Users, &summary.Guilds, &summary.Memberships, &summary.Accounts, &summary.Favorites)
	return summary, err
}
