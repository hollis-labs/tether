//go:build !windows

package runtoken

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateKeyCustodyRestartAndUnsafeExistingPaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "signing.key")
	var a Authority
	key, err := a.Key(path)
	if err != nil {
		t.Fatalf("private key path %q refused: %v", path, err)
	}
	var b Authority
	again, err := b.Key(path)
	if err != nil || !key.Equal(again) {
		t.Fatal("restart did not retain private key")
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Key(path); !errors.Is(err, ErrCustody) {
		t.Fatal("permissive key accepted")
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("unsafe existing key was repaired implicitly")
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Key(path); !errors.Is(err, ErrCustody) {
		t.Fatal("permissive directory accepted")
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(path), "symlink.key")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Key(link); !errors.Is(err, ErrCustody) {
		t.Fatal("symlink key accepted")
	}
	if len(key) != ed25519.PrivateKeySize {
		t.Fatal("invalid key")
	}
}
