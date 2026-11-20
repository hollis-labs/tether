//go:build !windows

package runtoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

var ErrCustody = errors.New("run signing key custody refused")

// Authority serializes creation in checked private custody, never the state DB.
// Every use rechecks opened inodes; unsafe existing permissions are not repaired.
type Authority struct{ mu sync.Mutex }

func (a *Authority) Key(path string) (ed25519.PrivateKey, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%w: invalid key path", ErrCustody)
	}
	dir := filepath.Dir(path)
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: open custody root", ErrCustody)
	}
	defer func() { _ = unix.Close(fd) }()
	parts := strings.Split(strings.TrimPrefix(dir, "/"), "/")
	protected := false
	createdCustody := false
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return nil, fmt.Errorf("%w: invalid directory component", ErrCustody)
		}
		if i == len(parts)-1 {
			err := unix.Mkdirat(fd, part, 0700)
			if err != nil && !errors.Is(err, unix.EEXIST) {
				return nil, fmt.Errorf("%w: create private directory", ErrCustody)
			}
			createdCustody = err == nil
		}
		next, err := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("%w: open private directory", ErrCustody)
		}
		var st unix.Stat_t
		err = unix.Fstat(next, &st)
		private := i == len(parts)-1
		safe := err == nil && (st.Uid == 0 || int(st.Uid) == os.Geteuid()) && (protected || st.Mode&0022 == 0 || st.Mode&unix.S_ISVTX != 0)
		if private {
			safe = err == nil && int(st.Uid) == os.Geteuid() && st.Mode&07777 == 0700
		}
		if !safe {
			_ = unix.Close(next)
			return nil, fmt.Errorf("%w: unsafe parent directory", ErrCustody)
		}
		if int(st.Uid) == os.Geteuid() && st.Mode&0077 == 0 {
			protected = true
		}
		_ = unix.Close(fd)
		fd = next
	}
	name := filepath.Base(path)
	keyFD, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		if !createdCustody {
			return nil, fmt.Errorf("%w: private key missing in retained custody", ErrCustody)
		}
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			return nil, fmt.Errorf("%w: generate key", ErrCustody)
		}
		create, e := unix.Openat(fd, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if e == nil {
			f := os.NewFile(uintptr(create), "run-signing-key") //nolint:gosec // G115: descriptor from successful openat.
			_, writeErr := f.Write(key)
			syncErr := f.Sync()
			closeErr := f.Close()
			if writeErr != nil || syncErr != nil || closeErr != nil || unix.Fsync(fd) != nil {
				return nil, fmt.Errorf("%w: write key", ErrCustody)
			}
		} else if !errors.Is(e, unix.EEXIST) {
			return nil, fmt.Errorf("%w: exclusive key creation", ErrCustody)
		}
		keyFD, err = unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: open key", ErrCustody)
	}
	f := os.NewFile(uintptr(keyFD), "run-signing-key") //nolint:gosec // G115: descriptor from successful openat.
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(keyFD, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&07777 != 0600 || int(st.Uid) != os.Geteuid() || st.Nlink != 1 {
		return nil, fmt.Errorf("%w: unsafe key inode", ErrCustody)
	}
	raw, err := io.ReadAll(io.LimitReader(f, ed25519.PrivateKeySize+1))
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("%w: invalid key length", ErrCustody)
	}
	key := ed25519.PrivateKey(raw)
	if !key.Equal(ed25519.NewKeyFromSeed(raw[:ed25519.SeedSize])) {
		return nil, fmt.Errorf("%w: invalid key material", ErrCustody)
	}
	return key, nil
}
