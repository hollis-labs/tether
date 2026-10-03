//go:build unix

package config

import "syscall"

// agentCanWrite reports whether a process running as this user, which is who a
// protected agent runs as, could create entries in dir: access(2) with W_OK and
// X_OK. The answer is the kernel's, so it accounts for modes, ownership, ACLs and
// a read-only file system.
func agentCanWrite(dir string) bool {
	const wOK, xOK = 0x2, 0x1
	return syscall.Access(dir, wOK|xOK) == nil
}
