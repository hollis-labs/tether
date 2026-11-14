package datadir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveRequiresTheVariable(t *testing.T) {
	if _, err := Resolve(env(nil)); !errors.Is(err, ErrUnset) {
		t.Fatalf("Resolve without %s = %v, want ErrUnset", Env, err)
	}
	base := t.TempDir()
	d, err := Resolve(env(map[string]string{Env: base}))
	if err != nil || string(d) != base {
		t.Fatalf("Resolve = %q, %v", d, err)
	}
}

func TestPrepareAndPrivateFile(t *testing.T) {
	d := Dir(filepath.Join(t.TempDir(), "data"))
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(string(d), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(string(d)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir mode %v, want 0700", fi.Mode().Perm())
	}
	db := filepath.Join(string(d), "nested", "tether.db")
	if err := d.PrivateFile(db); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(db, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.PrivateFile(db); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(db); fi.Mode().Perm() != 0o600 {
		t.Fatalf("database mode %v, want 0600", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(filepath.Dir(db)); fi.Mode().Perm() != 0o700 {
		t.Fatalf("parent mode %v, want 0700", fi.Mode().Perm())
	}
}

func TestAnchor(t *testing.T) {
	d := Dir("/data/tether")
	for _, c := range []struct{ p, want string }{
		{"", "/data/tether/tether.db"},
		{"tether.db", "/data/tether/tether.db"},
		{"db/tether.db", "/data/tether/db/tether.db"},
		{"/elsewhere/t.db", "/elsewhere/t.db"},
	} {
		got, err := d.Anchor("tether.db", c.p)
		if err != nil || got != c.want {
			t.Errorf("Anchor(%q) = %q, %v; want %q", c.p, got, err, c.want)
		}
	}
	if _, err := d.Anchor("tether.db", "../old/tether.db"); err == nil {
		t.Fatal("a relative path leaving the data directory must be refused")
	}
}
