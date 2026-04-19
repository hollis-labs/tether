//go:build unix

package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
)

// removeStaleSocket deletes the file at path only if it is a Unix socket.
// Any other file type (or a permission error) is surfaced so we don't
// accidentally clobber a regular file at the socket location.
func removeStaleSocket(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("listen_addr path %q exists and is not a socket", path)
	}
	return os.Remove(path)
}
