package config

import (
	"os"
	"path/filepath"
	"strings"
)

// Expand expands ~ and environment variables, then returns an absolute path.
func Expand(p string) string {
	if p == "" {
		return p
	}
	if strings.HasPrefix(p, "~") {
		home, _ := os.UserHomeDir()
		p = filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	p = os.ExpandEnv(p)
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

// maxSymlinkHops bounds RealPath's manual following of dangling symlinks.
const maxSymlinkHops = 40

// RealPath resolves path through symlinks, including a path that does not
// exist yet: the longest existing prefix is resolved and the rest is appended,
// and a dangling symlink in the prefix is followed to where it would land. So
// a path that goes through a symlink to a protected directory resolves into it
// even when the file at the end is new (a write there creates it inside).
// A relative path stays relative, cleaned. It never fails: where nothing
// resolves, it returns the cleaned path.
func RealPath(path string) string {
	return realPath(filepath.Clean(path), 0)
}

func realPath(p string, hops int) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	rest := ""
	cur := p
	for {
		if hops > maxSymlinkHops {
			return p
		}
		if fi, err := os.Lstat(cur); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			// A symlink whose target does not exist: follow it by hand.
			target, err := os.Readlink(cur)
			if err != nil {
				return p
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(cur), target)
			}
			return realPath(filepath.Join(filepath.Clean(target), rest), hops+1)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
	}
}

// PathWithin reports whether path is dir or lies beneath it. Both are compared
// as given: resolve them with RealPath first when symlinks matter.
func PathWithin(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}
