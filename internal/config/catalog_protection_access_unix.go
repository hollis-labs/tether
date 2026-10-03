//go:build unix

package config

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
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

// agentGetsPast says how an agent running as this user could create entries in
// dir although agentCanWrite(dir) is false, or "" when nothing it can do gets
// past: it examines every directory from dir up to / (see bypassVia). An error
// means the chain could not be examined, and the caller fails closed.
func agentGetsPast(dir string) (string, error) {
	cur, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", err
	}
	var chain []dirFacts
	for {
		info, err := os.Stat(cur)
		if err != nil {
			return "", err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return "", fmt.Errorf("cannot tell who owns %s", cur)
		}
		chain = append(chain, dirFacts{path: cur, uid: st.Uid, sticky: info.Mode()&fs.ModeSticky != 0, writable: agentCanWrite(cur)})
		parent := filepath.Dir(cur)
		if parent == cur {
			return bypassVia(chain, uint32(os.Geteuid())), nil //nolint:gosec // a uid is never negative
		}
		cur = parent
	}
}
