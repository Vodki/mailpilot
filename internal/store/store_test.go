package store

import (
	"context"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestMigrateIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	n, err := s.Migrate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != LatestVersion() || n < 1 {
		t.Fatalf("applied %d, latest %d", n, LatestVersion())
	}
	n, err = s.Migrate(ctx)
	if err != nil || n != 0 {
		t.Fatalf("second migrate applied %d, err %v", n, err)
	}
	v, _ := s.Version(ctx)
	if v != LatestVersion() {
		t.Fatalf("version %d", v)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.DB().ExecContext(ctx, "PRAGMA user_version = 999"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(ctx); err == nil {
		t.Fatal("expected error on newer schema")
	}
}

func TestSettingsAndAudit(t *testing.T) {
	ctx := context.Background()
	s := openTemp(t)
	if _, err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := s.GetSetting(ctx, "paused"); err != nil || v != "" {
		t.Fatalf("absent setting: %q %v", v, err)
	}
	for _, v := range []string{"true", "false"} {
		if err := s.SetSetting(ctx, "paused", v); err != nil {
			t.Fatal(err)
		}
		if got, _ := s.GetSetting(ctx, "paused"); got != v {
			t.Fatalf("got %q want %q", got, v)
		}
	}
	if err := s.Audit(ctx, "telegram:42", "rejected_sender", "update:7"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.DB().QueryRowContext(ctx, "SELECT count(*) FROM audit_log").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows %d %v", n, err)
	}
}

func TestPragmas(t *testing.T) {
	s := openTemp(t)
	var mode string
	var fk int
	_ = s.DB().QueryRow("PRAGMA journal_mode").Scan(&mode)
	_ = s.DB().QueryRow("PRAGMA foreign_keys").Scan(&fk)
	if mode != "wal" || fk != 1 {
		t.Fatalf("journal_mode=%s foreign_keys=%d", mode, fk)
	}
}
