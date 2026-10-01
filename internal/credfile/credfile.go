// Package credfile reads a credential from a private file, so the catalog YAML
// that names it does not have to carry the secret (CW-20261001-0229).
//
// A credential file is checked before its contents are used:
//
//   - the path is absolute, or starts with ~/ (the operator's home);
//   - it is a regular file, owned by the user running Tether;
//   - it is not readable by group or others (mode 0600 or tighter, as ssh
//     requires of a private key), so a loosely-permissioned copy is refused
//     rather than trusted;
//   - if the path goes through a symlink, the file it resolves to lies inside
//     one of the allowed roots (the catalog directory and the operator's home),
//     so a link planted in the catalog cannot point Tether at an arbitrary file;
//   - it is not empty and not larger than MaxSize.
//
// An error names the path and the rule that failed. It never contains the
// file's contents.
//
// These checks catch a misconfigured or misdirected file. They are not a
// defense against another process running as the same user, which can read the
// file and the environment the credential is passed in; see docs/secrets.md.
// Only the credential file itself is checked: a parent directory that others can
// write to is accepted, and so is the race of an ancestor directory being
// swapped for a symlink between the checks and the open, which needs write
// access to that ancestor.
package credfile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// MaxSize bounds a credential file. A token or key is a few hundred bytes;
// anything near this is a file that was named by mistake.
const MaxSize = 64 << 10

var (
	// ErrNotAbsolute is returned for a relative path, which has no stable meaning
	// across the daemon, the CLI and a launched agent's proxy.
	ErrNotAbsolute = errors.New("credential file path must be absolute or start with ~/")
	// ErrNotRegular is returned when the path is a directory, device, socket or pipe.
	ErrNotRegular = errors.New("credential file is not a regular file")
	// ErrTooPermissive is returned when group or others can read the file.
	ErrTooPermissive = errors.New("credential file is readable by group or others")
	// ErrNotOwner is returned when the file belongs to another user.
	ErrNotOwner = errors.New("credential file is not owned by the current user")
	// ErrSymlinkEscape is returned when the path resolves through a symlink to a
	// file outside the allowed roots.
	ErrSymlinkEscape = errors.New("credential file resolves through a symlink outside the catalog and home directories")
	// ErrEmpty is returned for a file with no credential in it.
	ErrEmpty = errors.New("credential file is empty")
	// ErrTooLarge is returned for a file larger than MaxSize.
	ErrTooLarge = errors.New("credential file is too large")
)

// Options say where a symlinked credential file may resolve to.
type Options struct {
	// Roots are directories a symlink may resolve into: the catalog directory and
	// the operator's home. A path that involves no symlink is not restricted to
	// them.
	Roots []string
}

// Read returns the credential stored in path, with surrounding whitespace (the
// trailing newline an editor adds) removed.
func Read(path string, opts Options) (string, error) {
	p, err := expand(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", fmt.Errorf("credential file %s: %w", p, unwrapPathError(err))
	}
	if resolved != p && !within(resolved, resolveRoots(opts.Roots)) {
		return "", fmt.Errorf("%s: %w (it resolves to %s)", p, ErrSymlinkEscape, resolved)
	}

	// Look at what is there BEFORE opening it. Opening a FIFO with no writer
	// blocks forever, and a device can do worse; neither is a credential file.
	li, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("credential file %s: %w", p, unwrapPathError(err))
	}
	if !li.Mode().IsRegular() {
		return "", fmt.Errorf("%s: %w", p, ErrNotRegular)
	}

	f, err := openNoFollow(resolved)
	if err != nil {
		return "", fmt.Errorf("credential file %s: %w", p, unwrapPathError(err))
	}
	defer func() { _ = f.Close() }()

	// Check the open file as well, not only the path: if the path was swapped for
	// something else after the Lstat above, the mode and type checked here are
	// those of the file actually read. openNoFollow does not block on a FIFO, so
	// even that swap cannot hang the open.
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("credential file %s: %w", p, unwrapPathError(err))
	}
	if !fi.Mode().IsRegular() {
		return "", fmt.Errorf("%s: %w", p, ErrNotRegular)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return "", fmt.Errorf("%s: %w (mode %04o); run chmod 600 on it", p, ErrTooPermissive, perm)
	}
	if !ownedByCurrentUID(fi) {
		return "", fmt.Errorf("%s: %w", p, ErrNotOwner)
	}

	body, err := io.ReadAll(io.LimitReader(f, MaxSize+1))
	if err != nil {
		return "", fmt.Errorf("credential file %s: read: %w", p, unwrapPathError(err))
	}
	if len(body) > MaxSize {
		return "", fmt.Errorf("%s: %w (limit %d bytes)", p, ErrTooLarge, MaxSize)
	}
	secret := strings.TrimSpace(string(body))
	if secret == "" {
		return "", fmt.Errorf("%s: %w", p, ErrEmpty)
	}
	return secret, nil
}

// expand turns path into a clean absolute path, expanding a leading ~/.
func expand(path string) (string, error) {
	switch {
	case path == "~" || strings.HasPrefix(path, "~/"):
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", fmt.Errorf("credential file %s: cannot resolve ~: no home directory", path)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~"))
	case !filepath.IsAbs(path):
		return "", fmt.Errorf("%q: %w", path, ErrNotAbsolute)
	}
	return filepath.Clean(path), nil
}

// resolveRoots resolves each root's own symlinks, so a real path can be compared
// with them. A root that does not exist is dropped: nothing resolves into it.
func resolveRoots(roots []string) []string {
	var out []string
	for _, r := range roots {
		if r == "" {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(r); err == nil {
			out = append(out, resolved)
		}
	}
	return out
}

// within reports whether path is root or lies under one of roots.
func within(path string, roots []string) bool {
	for _, root := range roots {
		rel, err := filepath.Rel(root, path)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// unwrapPathError drops the *PathError's own copy of the path, which the caller
// has already put in its message, while keeping the cause for errors.Is.
func unwrapPathError(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
