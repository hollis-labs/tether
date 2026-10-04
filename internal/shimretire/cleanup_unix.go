//go:build linux || darwin

package shimretire

import (
	"os"
	"path"
	"strings"
	"syscall"
)

func fileIdentity(info os.FileInfo) FileIdentity {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return FileIdentity{}
	}
	kind := "other"
	if info.Mode().IsRegular() {
		if st.Nlink != 1 {
			return FileIdentity{}
		}
		kind = "regular"
	}
	if info.IsDir() {
		kind = "directory"
	}
	return FileIdentity{Device: uint64(st.Dev), Inode: st.Ino, Kind: kind} //nolint:unconvert // Dev has a different width on Darwin.
}
func parentPath(p string) string { return path.Dir(p) }
func confinedParents(root *os.Root, p string) bool {
	parts := strings.Split(parentPath(p), "/")
	current := "."
	for _, part := range parts {
		if part == "." {
			continue
		}
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || fileIdentity(info).Kind != "directory" {
			return false
		}
	}
	return true
}
