package launchartifacts

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCodexNativeLinkPreservesCredentialSourceBoundary(t *testing.T) {
	for _, kind := range []string{"accepted", "missing", "foreign", "regular", "source_symlink", "source_directory_changed", "native_link_changed"} {
		t.Run(kind, func(t *testing.T) {
			source, native := t.TempDir(), t.TempDir()
			auth := filepath.Join(source, "auth.json")
			if err := os.WriteFile(auth, []byte("synthetic credential fixture"), 0600); err != nil {
				t.Fatal(err)
			}
			home, err := CaptureCodexHome(source)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = home.Close() })
			target := auth
			if kind == "foreign" {
				target = filepath.Join(t.TempDir(), "auth.json")
			}
			link := filepath.Join(native, "auth.json")
			if kind != "missing" {
				if kind == "regular" {
					err = os.WriteFile(link, []byte("synthetic"), 0600)
				} else {
					err = os.Symlink(target, link)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			switch kind {
			case "source_symlink":
				if err := os.Remove(auth); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(native, "foreign"), auth); err != nil {
					t.Fatal(err)
				}
			case "source_directory_changed":
				if err := os.Rename(source, source+"-retained"); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(source, 0700); err != nil {
					t.Fatal(err)
				}
			case "native_link_changed":
				if err := home.ValidateNativeLink(native); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(filepath.Join(t.TempDir(), "auth.json"), link); err != nil {
					t.Fatal(err)
				}
			}
			err = home.ValidateNativeLink(native)
			if (err == nil) != (kind == "accepted") {
				t.Fatalf("acceptance %s: %v", kind, err)
			}
		})
	}
}
