package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorNamesInvalidMCPGrant(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "global.yaml"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "projects"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "projects", "bad.yaml"), []byte("id: bad\nmcp:\n  servers: [Torque]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	result, cat := checkCatalog(root)
	if cat != nil || result.Name != "catalog-mcp-grants" || result.Status != statusFail || !strings.Contains(result.Message, `project "bad"`) || !strings.Contains(result.Message, `"Torque"`) {
		t.Fatalf("doctor result: %+v", result)
	}
}

func TestDoctorMCPGrantCatalogEnablementAndRedaction(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("global.yaml", "{}\n")
	write("projects/tangent.yaml", "id: tangent\nmcp:\n  servers: [tangent]\n")
	for _, enabled := range []bool{true, false} {
		state := "true"
		if !enabled {
			state = "false"
		}
		write("mcp-servers/tangent.yaml", "id: tangent\ntransport: http\nenabled: "+state+"\nurl: https://secret.example/private\ntoken: do-not-print-this-token\n")
		result, cat := checkCatalog(root)
		if enabled && (cat == nil || result.Status != statusOK) {
			t.Fatalf("enabled remote upstream is a valid grant despite local-confinement exclusion: %+v", result)
		}
		if !enabled && (cat != nil || result.Name != "catalog-mcp-grants" || result.Status != statusFail) {
			t.Fatalf("disabled upstream must fail: %+v", result)
		}
		text := result.Message + result.Remedy
		for _, sensitive := range []string{"https://secret.example/private", "do-not-print-this-token"} {
			if strings.Contains(text, sensitive) {
				t.Fatalf("doctor leaked entry credentials: %+v", result)
			}
		}
	}
}
