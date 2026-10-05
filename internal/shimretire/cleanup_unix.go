//go:build linux || darwin

package shimretire

import (
	"os"
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
