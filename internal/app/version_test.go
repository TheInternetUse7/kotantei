package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestVersionIsCredentialFreeAndDoesNotReadConfigurationOrCreateStorage(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "missing", "kotantei.db")
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"--config", filepath.Join(dir, "missing.json"), "version"}, &stdout, &stderr, envFor(database, ""))
	if code != 0 {
		t.Fatalf("version failed before reporting build identity: %d %s", code, stderr.String())
	}
	var report struct{ Version, License string }
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil || report.Version == "" || report.License != "AGPL-3.0-only" {
		t.Fatal("build identity or license missing", stdout.String(), err)
	}
	if _, err := os.Stat(filepath.Dir(database)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("version created storage", err)
	}
}

func TestVersionRejectsUnexpectedArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), []string{"version", "extra"}, &stdout, &stderr, nil); code != 64 {
		t.Fatal("version accepted unexpected arguments", code)
	}
}
