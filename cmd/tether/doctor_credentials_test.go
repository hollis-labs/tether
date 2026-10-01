package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorMCPCredentialFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "catalog")
	if err := os.MkdirAll(filepath.Join(dir, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "credential")
	const secret = "doctor-must-never-print-this-token"
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	yaml := []byte("id: test\ncommand: /must-not-run\ntoken: file://" + path + "\n")
	if err := os.WriteFile(filepath.Join(dir, "mcp-servers", "test.yaml"), yaml, 0600); err != nil {
		t.Fatal(err)
	}
	checks := checkMCPCredentialFiles(dir)
	if len(checks) != 1 || checks[0].Status != statusOK {
		t.Fatalf("unexpected pass: %+v", checks)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	checks = checkMCPCredentialFiles(dir)
	if len(checks) != 1 || checks[0].Status != statusFail || !strings.Contains(checks[0].Message, path) {
		t.Fatalf("unexpected fail: %+v", checks)
	}
	encoded, err := json.Marshal(checks)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatal("credential leaked in doctor JSON")
	}
}
