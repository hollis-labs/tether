//go:build !windows

package shimhost

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// PrivateDir refuses existing symlinks, foreign ownership or loose permissions.
func PrivateDir(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("private directory must be absolute")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	return checkPrivateDir(path)
}
func checkPrivateDir(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || int64(stat.Uid) != int64(os.Getuid()) {
		return fmt.Errorf("unsafe private directory")
	}
	return nil
}
func privateFile(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int64(stat.Uid) != int64(os.Getuid()) {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe private file")
	}
	return f, nil
}

// ReadPrivateJSON reads a bounded, owned, regular 0600 file without following a
// final symlink. The directory must be private and hidden from the provider.
func ReadPrivateJSON(path string, maxBytes int64, out any) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("private record path must be absolute")
	}
	if err := checkPrivateDir(filepath.Dir(path)); err != nil {
		return err
	}
	f, err := privateFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > maxBytes {
		return fmt.Errorf("private record exceeds limit")
	}
	return json.Unmarshal(b, out)
}

// WritePrivateJSON commits a whole record with file fsync, rename and directory
// fsync. On failure the previous record survives; no shared fixed temp name.
func WritePrivateJSON(path string, value any) error {
	dir := filepath.Dir(path)
	if err := PrivateDir(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		f, e := privateFile(path)
		if e != nil {
			return e
		}
		_ = f.Close()
	} else if !os.IsNotExist(err) {
		return err
	}
	commit, err := LockWait(context.Background(), filepath.Join(dir, "record.lock"), 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = commit.Close() }()
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".commit-")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(dir) //nolint:gosec // Owned private directory validated above; opened only for fsync.
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// Lock serializes placement/bridge ownership across processes. It never waits
// indefinitely: callers retry ErrWouldBlock against their context deadline.
func Lock(path string) (*os.File, error) {
	if err := PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int64(stat.Uid) != int64(os.Getuid()) {
		_ = f.Close()
		return nil, fmt.Errorf("unsafe lock file")
	}
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// LockWait bounds contention without converting an uncertain placement into retry.
func LockWait(ctx context.Context, path string, limit time.Duration) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		lock, err := Lock(path)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, fail("busy", "private record is busy")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// ClearCommitTemps serializes with every writer before deleting abandoned files.
func ClearCommitTemps(dir string) error {
	if err := checkPrivateDir(dir); err != nil {
		return err
	}
	lock, err := LockWait(context.Background(), filepath.Join(dir, "record.lock"), 5*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".commit-") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		f, err := privateFile(path)
		if err != nil {
			return err
		}
		_ = f.Close()
		if err = os.Remove(path); err != nil {
			return err
		}
	}
	return syncPrivateDir(dir)
}

func syncPrivateDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // Owned private directory checked before cleanup.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return f.Sync()
}
