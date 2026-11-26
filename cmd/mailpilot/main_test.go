package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVersionAndUsage(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "mailpilot") {
		t.Fatalf("version: code %d out %q", code, out.String())
	}
	if code := run(nil, &out, &errb); code != 2 {
		t.Fatalf("no args should exit 2, got %d", code)
	}
	if code := run([]string{"nope"}, &out, &errb); code != 2 {
		t.Fatalf("unknown command should exit 2, got %d", code)
	}
}

func TestMigrateCommand(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := "data_dir: " + dir + "\nimap: {username: a@b.c}\n"
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"migrate", "-config", cfgPath}, &out, &errb); code != 0 {
		t.Fatalf("migrate failed: %s", errb.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "mailpilot.db")); err != nil {
		t.Fatal(err)
	}
}

func TestCheckStoreOnly(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(cfgPath, []byte("data_dir: "+dir+"\nimap: {username: a@b.c}\n"), 0o600)
	var out, errb bytes.Buffer
	if code := run([]string{"check", "-config", cfgPath, "-only", "store"}, &out, &errb); code != 0 {
		t.Fatalf("check store: code %d\n%s%s", code, out.String(), errb.String())
	}
	if !strings.Contains(out.String(), "Base SQLite") {
		t.Fatalf("unexpected output %q", out.String())
	}
}

func TestSelectChecks(t *testing.T) {
	got, err := selectChecks("", "llm,todoist")
	if err != nil || strings.Join(got, ",") != "store,imap,smtp,telegram" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := selectChecks("bogus", ""); err == nil {
		t.Fatal("expected unknown check error")
	}
}
