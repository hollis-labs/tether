//go:build unix

package definitionresolve

import (
	"os"
	"syscall"
)

func openRegular(root *os.Root, name string) (*os.File, error) {
	// NOFOLLOW rejects a last-component link atomically; NONBLOCK avoids hanging
	// if a concurrently replaced file becomes a FIFO. os.Root confines ancestors.
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}
