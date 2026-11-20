package gatewaylaunch

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"strings"
)

// Do not silently fall back to a persistent temp directory. Linux tmpfs is
// required for the rotating gateway bearer; long-lived issuer keys stay out.
func memoryDir() (string, error) {
	var fs unix.Statfs_t
	if unix.Statfs("/dev/shm", &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		return "", errors.New("gateway credentials require tmpfs")
	}
	return os.MkdirTemp("/dev/shm", "tether-gateway-")
}

func validateMemoryPath(path string) error {
	dir := filepath.Dir(path)
	if !filepath.IsAbs(path) || path != filepath.Clean(path) || filepath.Base(path) != "gateway.token" || filepath.Dir(dir) != "/dev/shm" || !strings.HasPrefix(filepath.Base(dir), "tether-gateway-") {
		return errors.New("invalid gateway tmpfs path")
	}
	st, err := os.Lstat(dir)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0700 {
		return errors.New("gateway tmpfs directory unavailable")
	}
	var stat unix.Stat_t
	var fs unix.Statfs_t
	if unix.Lstat(dir, &stat) != nil || int(stat.Uid) != os.Geteuid() || unix.Statfs(dir, &fs) != nil || fs.Type != unix.TMPFS_MAGIC {
		return errors.New("gateway tmpfs ownership unavailable")
	}
	return nil
}
