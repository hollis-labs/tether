//go:build unix

package credfile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

const secret = "s3cr3t-token-value-0123456789"

// writeCred writes body to dir/name with the given mode and returns its path.
// It chmods after writing so the umask cannot loosen or tighten the result.
func writeCred(t *testing.T, dir, name, body string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func mustNotLeak(t *testing.T, err error) {
	t.Helper()
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Fatalf("error text exposes the credential: %v", err)
	}
}

func TestRead_ReturnsTrimmedContentOfAPrivateFile(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o600, 0o400} {
		p := writeCred(t, dir, "tok", "  "+secret+"\r\n\n", mode)
		got, err := Read(p, Options{})
		if err != nil {
			t.Fatalf("mode %04o: %v", mode, err)
		}
		if got != secret {
			t.Fatalf("mode %04o: got %q, want the trimmed credential", mode, got)
		}
	}
}

func TestRead_RefusesFilesGroupOrOthersCanRead(t *testing.T) {
	dir := t.TempDir()
	for _, mode := range []os.FileMode{0o640, 0o604, 0o644, 0o660, 0o666, 0o770} {
		p := writeCred(t, dir, "tok", secret, mode)
		got, err := Read(p, Options{})
		if !errors.Is(err, ErrTooPermissive) {
			t.Fatalf("mode %04o: err = %v, want ErrTooPermissive", mode, err)
		}
		if got != "" {
			t.Fatalf("mode %04o: returned %q alongside an error", mode, got)
		}
		if !strings.Contains(err.Error(), p) || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("mode %04o: error should name the file and the fix: %v", mode, err)
		}
		mustNotLeak(t, err)
	}
}

func TestRead_MissingFileIsAnErrorNamingThePath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent")
	_, err := Read(p, Options{})
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
	if !strings.Contains(err.Error(), p) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func TestRead_RefusesANonRegularFile(t *testing.T) {
	// t.TempDir() is 0700, so the directory passes the mode check and is refused
	// for what it is.
	if _, err := Read(t.TempDir(), Options{}); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("directory: err = %v, want ErrNotRegular", err)
	}
}

func TestRead_RefusesEmptyAndOversizedFiles(t *testing.T) {
	dir := t.TempDir()
	for _, body := range []string{"", " \n\t\r\n"} {
		p := writeCred(t, dir, "empty", body, 0o600)
		if _, err := Read(p, Options{}); !errors.Is(err, ErrEmpty) {
			t.Fatalf("body %q: err = %v, want ErrEmpty", body, err)
		}
	}
	big := writeCred(t, dir, "big", strings.Repeat("a", MaxSize+1), 0o600)
	if _, err := Read(big, Options{}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized: err = %v, want ErrTooLarge", err)
	}
	atLimit := writeCred(t, dir, "limit", strings.Repeat("a", MaxSize), 0o600)
	if _, err := Read(atLimit, Options{}); err != nil {
		t.Fatalf("a file of exactly MaxSize should be read: %v", err)
	}
}

func TestRead_RefusesARelativePath(t *testing.T) {
	_, err := Read("secrets/token", Options{})
	if !errors.Is(err, ErrNotAbsolute) {
		t.Fatalf("err = %v, want ErrNotAbsolute", err)
	}
}

func TestRead_ExpandsTildeToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	if err := os.MkdirAll(filepath.Join(home, ".tether", "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeCred(t, filepath.Join(home, ".tether", "secrets"), "tesseract.token", secret+"\n", 0o600)
	got, err := Read("~/.tether/secrets/tesseract.token", Options{})
	if err != nil || got != secret {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestRead_RefusesAFileOwnedBySomeoneElse(t *testing.T) {
	p := writeCred(t, t.TempDir(), "tok", secret, 0o600)
	prev := currentUID
	currentUID = func() int { return os.Getuid() + 1 }
	t.Cleanup(func() { currentUID = prev })
	_, err := Read(p, Options{})
	if !errors.Is(err, ErrNotOwner) {
		t.Fatalf("err = %v, want ErrNotOwner", err)
	}
	mustNotLeak(t, err)
}

func TestRead_SymlinkIntoAnAllowedRootIsFollowed(t *testing.T) {
	catalog := t.TempDir()
	target := writeCred(t, t.TempDir(), "real-token", secret, 0o600)
	// The target lives outside the catalog, so the link must be refused...
	link := filepath.Join(catalog, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(link, Options{Roots: []string{catalog}}); !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("link to a file outside the roots: err = %v, want ErrSymlinkEscape", err)
	}

	// ...while a link whose target is inside a root is fine, and so is naming that
	// root directly.
	inside := writeCred(t, catalog, "inside-token", secret, 0o600)
	link2 := filepath.Join(catalog, "link2")
	if err := os.Symlink(inside, link2); err != nil {
		t.Fatal(err)
	}
	got, err := Read(link2, Options{Roots: []string{catalog}})
	if err != nil || got != secret {
		t.Fatalf("link inside the catalog: got %q, %v", got, err)
	}

	roots := []string{t.TempDir(), filepath.Dir(target)}
	got, err = Read(link, Options{Roots: roots})
	if err != nil || got != secret {
		t.Fatalf("a root may be any allowed directory (the operator's home): got %q, %v", got, err)
	}
}

func TestRead_SymlinkedParentDirectoryCannotEscape(t *testing.T) {
	catalog := t.TempDir()
	outside := t.TempDir()
	writeCred(t, outside, "token", secret, 0o600)
	if err := os.Symlink(outside, filepath.Join(catalog, "dir")); err != nil {
		t.Fatal(err)
	}
	_, err := Read(filepath.Join(catalog, "dir", "token"), Options{Roots: []string{catalog}})
	if !errors.Is(err, ErrSymlinkEscape) {
		t.Fatalf("err = %v, want ErrSymlinkEscape", err)
	}
	mustNotLeak(t, err)
}

func TestRead_ARegularFileOutsideTheRootsIsAllowed(t *testing.T) {
	// The roots limit where a symlink may lead. A credential at a plain absolute
	// path (for example one a service manager mounts) is not a symlink.
	p := writeCred(t, t.TempDir(), "tok", secret, 0o600)
	got, err := Read(p, Options{Roots: []string{t.TempDir()}})
	if err != nil || got != secret {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestOpenNoFollow_RefusesASymlinkAtTheFinalComponent(t *testing.T) {
	dir := t.TempDir()
	target := writeCred(t, dir, "real", secret, 0o600)
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	f, err := openNoFollow(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("openNoFollow followed a symlink")
	}
	if !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("err = %v, want ELOOP", err)
	}
}

func TestWithin(t *testing.T) {
	roots := []string{"/a/b"}
	for path, want := range map[string]bool{
		"/a/b":         true,
		"/a/b/c":       true,
		"/a/b/c/d":     true,
		"/a/bc":        false,
		"/a":           false,
		"/a/b/../c":    false,
		"/elsewhere/x": false,
	} {
		if got := within(filepath.Clean(path), roots); got != want {
			t.Errorf("within(%q) = %v, want %v", path, got, want)
		}
	}
	if within("/a/b", nil) {
		t.Error("nothing is within an empty root list")
	}
}
