//go:build unix

package credfile

import (
	"os"
	"syscall"
)

// currentUID is the owner a credential file must have. A variable so a test can
// stand in for a file owned by someone else without being root.
var currentUID = os.Getuid

// openNoFollow opens path read-only. It refuses a final-component symlink, so a
// link swapped in after the path was resolved is not followed, and it does not
// block: opening a FIFO that has no writer returns at once instead of waiting
// for one, and Read then refuses it as not a regular file. O_NONBLOCK has no
// effect on reading a regular file.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: operator-configured credential path, validated by Read
}

func ownedByCurrentUID(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	return int(st.Uid) == currentUID()
}
