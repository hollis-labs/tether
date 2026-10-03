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
)

// agentCanWrite reports whether a process running as this user, which is who a
// protected agent runs as, could create entries in dir: access(2) with W_OK and
// X_OK. The answer is the kernel's, so it accounts for modes, ownership, ACLs and
// a read-only file system. It is not the whole answer for a directory that is not
// writable, because the agent is this user: see agentGetsPast.
func agentCanWrite(dir string) bool {
	const wOK, xOK = 0x2, 0x1
	return syscall.Access(dir, wOK|xOK) == nil
}

// inspectChain collects, for dir and every directory above it up to /, what decides
// whether an agent running as this user can get past it (see bypassVia). An error
// means the chain could not be examined, and the caller fails closed.
func inspectChain(dir string) ([]dirFacts, error) {
	cur, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	var chain []dirFacts
	for {
		info, err := os.Stat(cur)
		if err != nil {
			return nil, err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return nil, fmt.Errorf("cannot tell who owns %s", cur)
		}
		chain = append(chain, dirFacts{path: cur, uid: st.Uid, sticky: info.Mode()&fs.ModeSticky != 0, writable: agentCanWrite(cur)})
		parent := filepath.Dir(cur)
		if parent == cur {
			return chain, nil
		}
		cur = parent
	}
}

// agentGetsPast says how an agent running as this user could create entries in
// dir although agentCanWrite(dir) is false, or "" when nothing it can do gets
// past: it examines every directory from dir up to / (see bypassVia). An error
// means the chain could not be examined, and the caller fails closed.
func agentGetsPast(dir string) (string, error) {
	chain, err := inspectChain(dir)
	if err != nil {
		return "", err
	}
	return bypassVia(chain, uint32(os.Geteuid())), nil //nolint:gosec // a uid is never negative
}

// maxSymlinkWalk bounds the links followed while walking a path.
const maxSymlinkWalk = 40

// replaceableSymlink walks path as the loader and an agent's mkdir would, and says
// so when a symlink in it sits in a directory an agent running as this user can
// write or get past: the agent can unlink the link and put a real directory, with a
// planted layer, in its place. "" means every symlink in the path (there may be
// none) is out of its reach, or the walk reached something that does not exist.
func replaceableSymlink(path string) (string, error) {
	euid := uint32(os.Geteuid()) //nolint:gosec // a uid is never negative
	parts := strings.Split(filepath.Clean(path), string(filepath.Separator))
	cur, hops := string(filepath.Separator), 0
	for len(parts) > 0 {
		name := parts[0]
		parts = parts[1:]
		if name == "" {
			continue
		}
		next := filepath.Join(cur, name)
		info, err := os.Lstat(next)
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				return "", nil
			}
			return "", err
		}
		if info.Mode()&fs.ModeSymlink == 0 {
			cur = next
			continue
		}
		if hops++; hops > maxSymlinkWalk {
			return "", fmt.Errorf("too many symbolic links in %s", path)
		}
		target, err := os.Readlink(next)
		if err != nil {
			return "", err
		}
		chain, err := inspectChain(cur)
		if err != nil {
			return "", err
		}
		if way := bypassVia(chain, euid); way != "" {
			return fmt.Sprintf("%s -> %s: %s", next, target, way), nil
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(cur, target)
		}
		parts = append(strings.Split(filepath.Clean(target), string(filepath.Separator)), parts...)
		cur = string(filepath.Separator)
	}
	return "", nil
}
