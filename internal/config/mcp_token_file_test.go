package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStdioCredentialFileRejectsUnsafeInputs(t *testing.T) {
	file := filepath.Join(t.TempDir(), "credential")
	const fake = "fake-configuration-credential-only"
	if err := os.WriteFile(file, []byte(fake), 0600); err != nil {
		t.Fatal(err)
	}
	for _, e := range []MCPServerEntry{
		{Transport: "http", TokenFile: file},
		{TokenFile: file, Token: fake},
		{TokenFile: file, Args: []string{"--token", fake}},
		{TokenFile: file, Args: []string{"--token=" + fake}},
		{TokenFile: file, Args: []string{"--token-file", file}},
		{TokenFile: file, Args: []string{"--token-file=" + file}},
		{TokenFile: file, Args: []string{"other=" + fake}},
		{TokenFile: "relative"},
		{TokenFile: file + "-missing"},
	} {
		_, _, err := e.StdioCredentialArgs()
		if err == nil {
			t.Fatal("unsafe input accepted")
		}
		if strings.Contains(err.Error(), fake) {
			t.Fatal("error contains credential")
		}
	}
}

func TestTokenFileCatalogCheckAndTransportValidation(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "credential")
	if err := os.WriteFile(file, []byte("fake-config-token"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "mcp-servers"), 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "mcp-servers", "fixture.yaml")
	for _, transport := range []string{"stdio", "http"} {
		if err := os.WriteFile(p, []byte("id: fixture\ntransport: "+transport+"\ncommand: /must-not-run\ntoken_file: "+file+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		entries, err := LoadMCPServers(root)
		if transport == "http" {
			if err == nil {
				t.Fatal("HTTP ignored unsupported token_file")
			}
		} else {
			if err != nil || len(entries) != 1 || entries[0].TokenFile != file {
				t.Fatalf("stdio catalog: %v", err)
			}
		}
		checks, err := CheckMCPServerCredentialFiles(root)
		if err != nil || len(checks) != 1 || checks[0].Field != "token_file" {
			t.Fatalf("doctor check: %v", err)
		}
		if (checks[0].Err != nil) != (transport == "http") {
			t.Fatal("doctor validation disagrees with spawn")
		}
	}
}
