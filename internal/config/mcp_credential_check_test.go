//go:build unix

package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hollis-labs/tether/internal/credfile"
)

func TestCheckMCPServerCredentialFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "catalog")
	good := credFile(t, home, "good", 0600)
	loose := credFile(t, home, "loose", 0644)
	outside := credFile(t, t.TempDir(), "outside", 0600)
	link := filepath.Join(home, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(home, "pipe")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, ref string
		want      error
	}{
		{"good", good, nil},
		{"missing", filepath.Join(home, "missing"), fs.ErrNotExist},
		{"loose", loose, credfile.ErrTooPermissive},
		{"escape", link, credfile.ErrSymlinkEscape},
		{"fifo", fifo, credfile.ErrNotRegular},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			write(t, filepath.Join(dir, "mcp-servers", "test.yaml"), fmt.Sprintf("id: test\ncommand: /must-not-run\ntoken: %q\nurl: %q\nargs: [%q]\nenv:\n  KEY: %q\n", "file://"+tc.ref, "file://"+tc.ref, "file://"+tc.ref, "file://"+tc.ref))
			checks, err := CheckMCPServerCredentialFiles(dir)
			if err != nil {
				t.Fatal(err)
			}
			gotFields := map[string]bool{}
			for _, check := range checks {
				gotFields[check.Field] = true
				if check.ServerID != "test" || !errors.Is(check.Err, tc.want) {
					t.Fatalf("unexpected file check: %+v", check)
				}
				if check.Err != nil && strings.Contains(check.Err.Error(), fileCredSecret) {
					t.Fatal("credential leaked")
				}
			}
			for _, field := range []string{"token", "url", "args[0]", "env.KEY"} {
				if !gotFields[field] {
					t.Fatalf("field %s was not checked", field)
				}
			}
		})
	}
}

func TestCheckMCPServerCredentialFilesSkipsDisabledAndExpandedReferences(t *testing.T) {
	stub := &stubResolver{}
	withResolver(t, stub)
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, "catalog")
	t.Setenv("FILE_REF", "file://"+filepath.Join(home, "absent"))
	write(t, filepath.Join(dir, "mcp-servers", "disabled.yaml"), "id: off\nenabled: false\ntoken: file:///absent\n")
	write(t, filepath.Join(dir, "mcp-servers", "env.yaml"), "id: env\ntoken: ${FILE_REF}\n")
	write(t, filepath.Join(dir, "mcp-servers", "helper.yaml"), "id: helper\ntoken: helper://must-not-run/credential\n")
	checks, err := CheckMCPServerCredentialFiles(dir)
	if len(stub.seen) != 0 {
		t.Fatal("doctor called a secret helper")
	}
	if err != nil || len(checks) != 0 {
		t.Fatalf("unexpected checks: %+v, %v", checks, err)
	}
}
