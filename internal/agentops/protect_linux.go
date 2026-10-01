//go:build linux

package agentops

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/hollis-labs/tether/internal/launchprofile"
)

// maxAncestors bounds the walk from a directory up to the file system root. A
// path deeper than this is refused, which is fail-closed.
const maxAncestors = 4096

// fileID is a file's identity: which device, and which inode on it. Two paths
// name the same directory exactly when their identities are equal, whatever
// symlinks or mounts lead to them.
type fileID struct{ dev, ino uint64 }

func idOf(st *syscall.Stat_t) fileID {
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)} //nolint:unconvert // Dev and Ino are not uint64 on every GOARCH
}

// protectedIDs is the identity of each protected directory that exists. One
// that does not exist cannot be written into.
func protectedIDs(protected []string) []fileID {
	var ids []fileID
	for _, p := range protected {
		fi, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			ids = append(ids, idOf(st))
		}
	}
	return ids
}

func openat(dirfd int, name string, flags int, mode uint32) (int, error) {
	for {
		fd, err := syscall.Openat(dirfd, name, flags|syscall.O_CLOEXEC, mode)
		if !errors.Is(err, syscall.EINTR) {
			return fd, err
		}
	}
}

// insideProtected reports whether the open directory fd is one of the protected
// directories or lies beneath one. It walks up from fd through ".", comparing
// each directory's identity with the protected ones, so it judges the directory
// the descriptor holds and not any path that names it, and cannot be fooled by
// a symlink re-pointed while it runs.
func insideProtected(fd int, ids []fileID) (bool, error) {
	cur, owned := fd, false
	defer func() {
		if owned {
			_ = syscall.Close(cur)
		}
	}()
	for range maxAncestors {
		var st syscall.Stat_t
		if err := syscall.Fstat(cur, &st); err != nil {
			return false, err
		}
		id := idOf(&st)
		for _, p := range ids {
			if p == id {
				return true, nil
			}
		}
		parent, err := openat(cur, "..", syscall.O_RDONLY|syscall.O_DIRECTORY, 0)
		if err != nil {
			return false, err
		}
		var pst syscall.Stat_t
		if err := syscall.Fstat(parent, &pst); err != nil {
			_ = syscall.Close(parent)
			return false, err
		}
		if idOf(&pst) == id { // the root is its own parent
			_ = syscall.Close(parent)
			return false, nil
		}
		if owned {
			_ = syscall.Close(cur)
		}
		cur, owned = parent, true
	}
	return false, fmt.Errorf("more than %d ancestors", maxAncestors)
}

// judge refuses a directory in a protected one, and one it cannot place:
// failing to tell is not a reason to write.
func judge(fd int, dir string, ids []fileID) error {
	inside, err := insideProtected(fd, ids)
	if err != nil {
		return fmt.Errorf("%w: could not verify %s: %w", ErrProtected, dir, err)
	}
	if inside {
		return fmt.Errorf("%w: %s", ErrProtected, dir)
	}
	return nil
}

// openGuardedDir opens the directory dir for writing into, and returns a
// descriptor for it that has been judged not to be, or be inside, a protected
// directory. The longest existing prefix of dir is opened in one step, so the
// descriptor refers to one directory from then on, whatever the paths do. With
// create, the missing components are made relative to it, without following a
// symlink, and each is judged as it is opened.
func openGuardedDir(dir string, create bool, protected []string) (int, error) {
	ids := protectedIDs(protected)
	dir = filepath.Clean(dir)
	prefix := dir
	var missing []string
	for {
		_, err := os.Stat(prefix)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return -1, err
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			break
		}
		missing = append([]string{filepath.Base(prefix)}, missing...)
		prefix = parent
	}
	if len(missing) > 0 && !create {
		return -1, &fs.PathError{Op: "open", Path: dir, Err: syscall.ENOENT}
	}
	fd, err := syscall.Open(prefix, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return -1, &fs.PathError{Op: "open", Path: prefix, Err: err}
	}
	if err := judge(fd, prefix, ids); err != nil {
		_ = syscall.Close(fd)
		return -1, err
	}
	for _, name := range missing {
		if err := syscall.Mkdirat(fd, name, 0o750); err != nil && !errors.Is(err, syscall.EEXIST) {
			_ = syscall.Close(fd)
			return -1, &fs.PathError{Op: "mkdir", Path: filepath.Join(prefix, name), Err: err}
		}
		next, err := openat(fd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(fd)
		if err != nil {
			return -1, &fs.PathError{Op: "open", Path: filepath.Join(prefix, name), Err: err}
		}
		fd = next
		if err := judge(fd, dir, ids); err != nil {
			_ = syscall.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

// createFileGuarded writes body to a new file at path, relative to the open,
// judged directory, and never through a symlink.
func createFileGuarded(path string, body []byte, protected []string) error {
	dfd, err := openGuardedDir(filepath.Dir(path), true, protected)
	if err != nil {
		return err
	}
	defer func() { _ = syscall.Close(dfd) }()
	fd, err := openat(dfd, filepath.Base(path), syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		if errors.Is(err, syscall.EEXIST) {
			return fmt.Errorf("%w at %s", ErrExists, path)
		}
		return &fs.PathError{Op: "open", Path: path, Err: err}
	}
	return writeAndClose(os.NewFile(uintptr(fd), path), body)
}

// updateFileGuarded reads and rewrites the file at path through the open,
// judged directory. A file that is itself a symlink is refused: where it points
// is not something this can judge.
func updateFileGuarded(path string, p Params, protected []string) (launchprofile.LaunchProfile, error) {
	dfd, err := openGuardedDir(filepath.Dir(path), false, protected)
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	defer func() { _ = syscall.Close(dfd) }()
	name := filepath.Base(path)

	rfd, err := openat(dfd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return launchprofile.LaunchProfile{}, symlinkAware(path, err)
	}
	rf := os.NewFile(uintptr(rfd), path)
	fi, err := rf.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	var data []byte
	if err == nil {
		data, err = io.ReadAll(rf)
	}
	_ = rf.Close()
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	a, err := patchAgent(path, data, p)
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	body, err := marshalAgent(a)
	if err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	wfd, err := openat(dfd, name, syscall.O_WRONLY|syscall.O_TRUNC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return launchprofile.LaunchProfile{}, symlinkAware(path, err)
	}
	if err := writeAndClose(os.NewFile(uintptr(wfd), path), body); err != nil {
		return launchprofile.LaunchProfile{}, err
	}
	return a, nil
}

func symlinkAware(path string, err error) error {
	if errors.Is(err, syscall.ELOOP) {
		return fmt.Errorf("%s is a symlink, and a write that must stay out of the protected directories does not follow one: %w", path, err)
	}
	return &fs.PathError{Op: "open", Path: path, Err: err}
}

func writeAndClose(f *os.File, body []byte) error {
	_, err := f.Write(body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}
