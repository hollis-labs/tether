package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
)

func TestMCPExpandedHelperReferencesStayLiteral(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, scheme := range []string{"keychain", "helper"} {
		t.Run(scheme, func(t *testing.T) {
			ref := scheme + "://test/credential"
			t.Setenv("STEER_MCP_REFERENCE", ref)
			stub := &stubResolver{values: map[string]string{ref: "must-not-be-resolved"}}
			withResolver(t, stub)
			dir := t.TempDir()
			write(t, filepath.Join(dir, "mcp-servers", "test.yaml"), "id: test\ntransport: stdio\ncommand: /must-not-run\ntoken: ${STEER_MCP_REFERENCE}\nurl: ${STEER_MCP_REFERENCE}\nargs: [\"${STEER_MCP_REFERENCE}\"]\nenv:\n  KEY: ${STEER_MCP_REFERENCE}\n")
			entries, err := LoadMCPServers(dir)
			if err != nil {
				t.Fatal(err)
			}
			entry := entries[0]
			for _, value := range []string{entry.Token, entry.URL, entry.Args[0], entry.Env["KEY"]} {
				if value != ref {
					t.Fatal("environment expansion activated a secret reference")
				}
			}
			if len(stub.seen) != 0 {
				t.Fatal("environment expansion called a secret helper")
			}
			if slices.Contains(entry.ArgumentRedactionValues(), "must-not-be-resolved") {
				t.Fatal("resolved an un-authored reference")
			}
		})
	}
}

func TestMCPOperatorAuthoredHelperReferencesStillResolve(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, scheme := range []string{"keychain", "helper"} {
		t.Run(scheme, func(t *testing.T) {
			stub := &stubResolver{values: map[string]string{}}
			for _, field := range []string{"token", "url", "arg", "env"} {
				stub.values[scheme+"://test/"+field] = "resolved-" + field
			}
			withResolver(t, stub)
			dir := t.TempDir()
			write(t, filepath.Join(dir, "mcp-servers", "test.yaml"), fmt.Sprintf("id: test\ntransport: stdio\ncommand: /must-not-run\ntoken: %s://test/token\nurl: %s://test/url\nargs: [%s://test/arg]\nenv:\n  KEY: %s://test/env\n", scheme, scheme, scheme, scheme))
			entries, err := LoadMCPServers(dir)
			if err != nil {
				t.Fatal(err)
			}
			entry := entries[0]
			for field, value := range map[string]string{"token": entry.Token, "url": entry.URL, "arg": entry.Args[0], "env": entry.Env["KEY"]} {
				if value != "resolved-"+field || !slices.Contains(stub.seen, scheme+"://test/"+field) {
					t.Fatalf("operator-authored %s did not resolve", field)
				}
			}
			if !slices.Contains(entry.ArgumentRedactionValues(), "resolved-arg") || !slices.Contains(entry.ArgumentRedactionValues(), "resolved-url") {
				t.Fatal("resolved argument or URL lost redaction")
			}
		})
	}
}
