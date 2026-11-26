// Package store owns the SQLite database: connection setup, versioned
// migrations and queries.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, no CGO
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Store wraps the database handle.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the database at path with WAL and foreign
// keys enabled. It does not migrate; call Migrate.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// SQLite allows a single writer; one connection avoids SQLITE_BUSY between
	// our own goroutines. Reads are cheap enough at this volume.
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle to sibling packages' tests.
func (s *Store) DB() *sql.DB { return s.db }

type migration struct {
	version int
	name    string
	sql     string
}

func loadMigrations() ([]migration, error) {
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return nil, err
	}
	var out []migration
	for _, e := range entries {
		name := e.Name()
		num, _, ok := strings.Cut(name, "_")
		if !ok || !strings.HasSuffix(name, ".sql") {
			return nil, fmt.Errorf("migration %q: expected NNNN_name.sql", name)
		}
		v, err := strconv.Atoi(num)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", name, err)
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return nil, err
		}
		out = append(out, migration{version: v, name: name, sql: string(body)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].version < out[j].version })
	for i, m := range out {
		if m.version != i+1 {
			return nil, fmt.Errorf("migrations must be numbered 1..n without gaps, found %s at position %d", m.name, i+1)
		}
	}
	return out, nil
}

// Version returns the current schema version (PRAGMA user_version).
func (s *Store) Version(ctx context.Context) (int, error) {
	var v int
	err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v)
	return v, err
}

// LatestVersion is the schema version embedded in this binary.
func LatestVersion() int {
	ms, err := loadMigrations()
	if err != nil {
		return -1
	}
	return len(ms)
}

// Migrate applies pending migrations, each in its own transaction. It
// refuses to run against a database newer than this binary.
func (s *Store) Migrate(ctx context.Context) (applied int, err error) {
	ms, err := loadMigrations()
	if err != nil {
		return 0, err
	}
	cur, err := s.Version(ctx)
	if err != nil {
		return 0, err
	}
	if cur > len(ms) {
		return 0, fmt.Errorf("database schema v%d is newer than this binary (v%d)", cur, len(ms))
	}
	for _, m := range ms[cur:] {
		if err := s.apply(ctx, m); err != nil {
			return applied, fmt.Errorf("migration %s: %w", m.name, err)
		}
		applied++
	}
	return applied, nil
}

func (s *Store) apply(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	// PRAGMA does not accept bound parameters; version is an int we control.
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSetting returns a value from the settings table, or "" if absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a value in the settings table.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		key, value)
	return err
}

// Audit records a security-relevant event. ref must be an identifier, never
// message content.
func (s *Store) Audit(ctx context.Context, actor, event, ref string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO audit_log(actor, event, ref) VALUES(?, ?, ?)", actor, event, ref)
	return err
}
