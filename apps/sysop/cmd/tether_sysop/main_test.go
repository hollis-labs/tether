package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCatalogYAMLFileCreatesBackupAndWritesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "providers", "demo.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir parent: %v", err)
	}
	if err := os.WriteFile(path, []byte("id: old\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	backupPath, err := writeCatalogYAMLFile(path, map[string]any{"id": "new"}, 0o644)
	if err != nil {
		t.Fatalf("writeCatalogYAMLFile: %v", err)
	}
	if backupPath == "" {
		t.Fatal("expected backup path")
	}
	backupBytes, err := os.ReadFile(backupPath)
	if err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if string(backupBytes) != "id: old\n" {
		t.Fatalf("backup contents = %q, want original contents", string(backupBytes))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(got), "id: new") {
		t.Fatalf("written file missing new content: %q", string(got))
	}
}

func TestBackupCatalogFileReturnsUniqueNames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	if err := os.WriteFile(path, []byte("version: 1\n"), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}

	first, err := backupCatalogFile(path)
	if err != nil {
		t.Fatalf("first backup: %v", err)
	}
	second, err := backupCatalogFile(path)
	if err != nil {
		t.Fatalf("second backup: %v", err)
	}
	if first == second {
		t.Fatalf("backup paths should be unique: %q", first)
	}
	if !pathExists(first) || !pathExists(second) {
		t.Fatalf("expected backup files to exist: %q %q", first, second)
	}
}

func TestDecodeJSONBodyRejectsUnknownFields(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(`{"id":"x","extra":true}`))
	var body struct {
		ID string `json:"id"`
	}
	err := decodeJSONBody(req, &body, false)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("expected unknown field error, got %v", err)
	}
}

func TestDecodeJSONBodyRejectsTrailingJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewBufferString(`{"id":"x"}{"extra":true}`))
	var body struct {
		ID string `json:"id"`
	}
	err := decodeJSONBody(req, &body, false)
	if err == nil || !strings.Contains(err.Error(), "single JSON value") {
		t.Fatalf("expected trailing JSON error, got %v", err)
	}
}
