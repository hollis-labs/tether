package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RealPath resolves a path through symlinks even where its last parts do not
// exist yet, so a path that goes through a symlink into a protected directory
// resolves into it (CW-20261001-0142 nit: symlink-to-catalog/newsub stayed
// exempt because only an existing path was resolved).
func TestRealPath(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "target")
	if err := os.MkdirAll(filepath.Join(target, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(base, "dangling")
	if err := os.Symlink(filepath.Join(target, "later"), dangling); err != nil {
		t.Fatal(err)
	}
	loop := filepath.Join(base, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ name, in, want string }{
		{"an existing path", filepath.Join(link, "sub"), filepath.Join(resolvedTarget, "sub")},
		{"a nonexistent path under a symlink", filepath.Join(link, "new", "deeper"), filepath.Join(resolvedTarget, "new", "deeper")},
		{"a dangling symlink, followed to where it would land", filepath.Join(dangling, "x"), filepath.Join(resolvedTarget, "later", "x")},
		{"a path with .. is cleaned first", filepath.Join(link, "sub", "..", "other"), filepath.Join(resolvedTarget, "other")},
		{"nothing resolves", "/nonexistent-root-xyz/a/b", "/nonexistent-root-xyz/a/b"},
		{"a relative path stays relative", "a/./b", "a/b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RealPath(tc.in); got != tc.want {
				t.Fatalf("RealPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	// A symlink loop terminates rather than spinning.
	done := make(chan string, 1)
	go func() { done <- RealPath(filepath.Join(loop, "x")) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("RealPath did not terminate on a symlink loop")
	}
}

func TestPathWithin(t *testing.T) {
	for _, tc := range []struct {
		dir, path string
		want      bool
	}{
		{"/a/b", "/a/b", true},
		{"/a/b", "/a/b/c/d", true},
		{"/a/b", "/a/bc", false},
		{"/a/b", "/a", false},
		{"/a/b", "/x/y", false},
	} {
		if got := PathWithin(tc.dir, tc.path); got != tc.want {
			t.Errorf("PathWithin(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
		}
	}
}
