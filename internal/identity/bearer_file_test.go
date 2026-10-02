package identity_test

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

func TestServiceBearerFileFailsClosed(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	path := filepath.Join(dir, "service.token")
	cases := []struct {
		name, content string
		valid         bool
	}{
		{"opaque", "upstream-issued.secret+/=\n", true},
		{"jwt", strings.Repeat("a", 300) + ".payload.signature", true},
		{"empty", "\n", false},
		{"header", "Bearer secret", false},
		{"injection", "secret\r\nX-Forwarded-User-Id: operator", false},
		{"padding", "abc=def", false},
		{"oversized", strings.Repeat("a", 4097), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			token, err := identity.ReadBearerTokenFile(path)
			if tc.valid && (err != nil || token != strings.TrimSpace(tc.content)) {
				t.Fatalf("valid credential rejected: %v", err)
			}
			if !tc.valid && (err == nil || token != "") {
				t.Fatal("invalid credential accepted")
			}
			if _, err := identity.ReadTokenFile(path); err == nil {
				t.Fatal("upstream credential accepted as Tether token")
			}
		})
	}
	if err := os.WriteFile(path, []byte("opaque-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(dir, "fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, unsafe := range []string{link, fifo, dir} {
		if token, err := identity.ReadBearerTokenFile(unsafe); err == nil || token != "" {
			t.Fatal("unsafe credential file accepted")
		}
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if token, err := identity.ReadBearerTokenFile(path); err == nil || token != "" {
		t.Fatal("public credential file accepted")
	}
}
