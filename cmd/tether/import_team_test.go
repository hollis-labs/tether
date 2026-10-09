package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportTeamRejectsUnknownManifestWithoutEchoingInput(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "manifest.json")
	if err := os.WriteFile(path, []byte(`{"accidental_credential":"owned-fake-do-not-echo"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newImportTeamCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"--manifest", path, "--output", filepath.Join(base, "catalog")})
	err := cmd.Execute()
	if err == nil || strings.Contains(err.Error()+output.String(), "owned-fake-do-not-echo") {
		t.Fatal("invalid manifest accepted or sensitive input echoed")
	}
	if _, err := os.Lstat(filepath.Join(base, "catalog")); !os.IsNotExist(err) {
		t.Fatal("invalid CLI input mutated output")
	}
}
