// Package datadir resolves Tether's data directory and keeps it private. The directory holds the
// database, the carried issuer auth/ directory, operator.token and file state (port plan section 3).
//
// TETHER_DATA_DIR is required: the new data home is a new directory beside the old data root, named
// explicitly by the service descriptor. There is no XDG fallback, because the old tree resolves its
// state through the same XDG application name and a default here could land on the old files.
package datadir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Env names the data directory. It is the only setting read from the environment.
const Env = "TETHER_DATA_DIR"

// ErrUnset is returned when Env is empty.
var ErrUnset = errors.New(Env + " is not set: name the data directory explicitly (a new directory beside the old data root, never the old one)")

// Dir is a resolved, absolute data directory.
type Dir string

// Resolve reads Env through getenv (os.Getenv outside tests). Nothing is created.
func Resolve(getenv func(string) string) (Dir, error) {
	v := getenv(Env)
	if v == "" {
		return "", ErrUnset
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", Env, err)
	}
	return Dir(abs), nil
}

// Prepare creates the directory 0700 when missing and tightens it when it exists.
func (d Dir) Prepare() error {
	if err := os.MkdirAll(string(d), 0o700); err != nil {
		return err
	}
	return os.Chmod(string(d), 0o700) //nolint:gosec // G302: 0700 is a directory mode
}

// Within reports whether p is d or below it (lexically).
func (d Dir) Within(p string) bool {
	rel, err := filepath.Rel(string(d), p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Anchor places a configured path in the data directory: empty is def joined onto d; an absolute path
// is the operator's explicit choice and is kept; a relative path is joined onto d, and refused if it
// climbs out through "..", which would put private data outside the 0700 directory.
func (d Dir) Anchor(def, p string) (string, error) {
	switch {
	case p == "":
		return filepath.Join(string(d), def), nil
	case filepath.IsAbs(p):
		return filepath.Clean(p), nil
	}
	joined := filepath.Join(string(d), p)
	if !d.Within(joined) {
		return "", fmt.Errorf("relative path %q leaves the data directory %s; use an absolute path to put it elsewhere", p, d)
	}
	return joined, nil
}

// PrivateFile creates the file at p with mode 0600 when missing (its parent directories 0700 when they
// are inside d) and tightens an existing one. SQLite gives its sidecar files the database file's mode.
func (d Dir) PrivateFile(p string) error {
	if d.Within(p) {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDWR, 0o600) //nolint:gosec // G304: p is operator configuration
	if err != nil {
		return err
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
