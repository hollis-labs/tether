package sshenroll

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/service"
	"golang.org/x/sys/unix"
)

type Receipt struct {
	OperationID         string    `json:"operation_id"`
	Target              string    `json:"ssh_target"`
	Authority           string    `json:"authority"`
	Version             string    `json:"version"`
	ArchiveSHA256       string    `json:"archive_sha256,omitempty"`
	RemotePort          int       `json:"remote_port"`
	Scopes              []string  `json:"scopes"`
	Phase               string    `json:"phase"`
	EnvironmentID       string    `json:"environment_id,omitempty"`
	GrantID             string    `json:"grant_id,omitempty"`
	DeviceID            string    `json:"device_id,omitempty"`
	CredentialReference string    `json:"credential_reference,omitempty"`
	Preflight           Preflight `json:"preflight"`
}

func (r Receipt) matches(o Options) bool {
	return r.Target == o.Target && r.Authority == o.Authority && r.Version == o.Version && r.RemotePort == o.RemotePort && reflect.DeepEqual(r.Scopes, o.Scopes)
}
func (r Receipt) workerRequest() WorkerRequest {
	return WorkerRequest{OperationID: r.OperationID, Authority: r.Authority, Version: r.Version, ArchiveSHA256: r.ArchiveSHA256, RemotePort: r.RemotePort, Scopes: r.Scopes, DeviceID: r.DeviceID}
}

type receipts struct{ root string }

// Existing ancestors must be real directories. Private leaves are owned by
// this user; a same-UID adversary remains outside the promised isolation model.
func privateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return problem("storage", "invalid-private-directory", "Choose an absolute clean private directory.")
	}
	parent := filepath.Dir(path)
	if parent != path {
		if info, err := os.Lstat(parent); errors.Is(err, os.ErrNotExist) {
			if err := privateDirectory(parent); err != nil {
				return err
			}
		} else if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return problem("storage", "unsafe-parent", "Private storage must not traverse a symlink or special file.")
		}
	}
	// Check all ancestors too, including an already existing immediate parent.
	for p := parent; ; p = filepath.Dir(p) {
		info, err := os.Lstat(p)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return problem("storage", "unsafe-parent", "Private storage must not traverse a symlink.")
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return problem("storage", "directory-unavailable", "Cannot prepare private enrollment storage.")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return problem("storage", "insecure-directory", "Enrollment storage must be a real current-user-owned 0700 directory.")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return problem("storage", "wrong-owner", "Enrollment storage must belong to the current user.")
	}
	return nil
}

func openPrivate(path string, flags int) (*os.File, error) {
	fd, err := unix.Open(path, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		v, ok := info.Sys().(*syscall.Stat_t)
		return v, ok
	}()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int(stat.Uid) != os.Geteuid() {
		_ = f.Close()
		return nil, problem("storage", "insecure-file", "Private files must be regular current-user-owned 0600 files.")
	}
	return f, nil
}

func openReceipts(ctx context.Context, root string) (*receipts, func(), error) {
	if err := privateDirectory(root); err != nil {
		return nil, nil, err
	}
	f, err := openPrivate(filepath.Join(root, ".lock"), unix.O_RDWR|unix.O_CREAT)
	if err != nil {
		return nil, nil, problem("receipt", "lock-unavailable", "Retain the enrollment state; its private lock could not be opened.")
	}
	for {
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, nil, problem("receipt", "lock-unavailable", "Cannot acquire the enrollment lock.")
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
	return &receipts{root}, func() { _ = unix.Flock(int(f.Fd()), unix.LOCK_UN); _ = f.Close() }, nil
}

func (s *receipts) load() (Receipt, error) {
	var r Receipt
	f, err := openPrivate(filepath.Join(s.root, "receipt.json"), unix.O_RDONLY)
	if errors.Is(err, os.ErrNotExist) {
		return r, io.EOF
	}
	if err != nil {
		return r, problem("receipt", "unreadable-receipt", "Retain and inspect the private receipt; do not replace it.")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (64<<10)+1))
	if err != nil || len(data) > 64<<10 || json.Unmarshal(data, &r) != nil || !validReceipt(r, s.root) {
		return Receipt{}, problem("receipt", "invalid-receipt", "Retain the receipt rather than creating a replacement operation.")
	}
	return r, nil
}
func (s *receipts) save(r Receipt) error {
	data, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return privateAtomic(s.root, "receipt.json", data, false)
}

func privateAtomic(root, name string, data []byte, exclusive bool) error {
	f, err := os.CreateTemp(root, ".candidate-")
	if err != nil {
		return problem("storage", "write-failed", "Private state could not be staged.")
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return problem("storage", "write-failed", "Private state could not be synced.")
	}
	target := filepath.Join(root, name)
	if exclusive {
		err = os.Link(path, target)
	} else {
		if _, e := os.Lstat(target); e == nil {
			existing, err := openPrivate(target, unix.O_RDONLY)
			if err != nil {
				return problem("storage", "unsafe-destination", "Retain the changed private destination.")
			}
			_ = existing.Close()
		} else if !errors.Is(e, os.ErrNotExist) {
			return problem("storage", "unsafe-destination", "Retain the inaccessible private destination.")
		}
		err = os.Rename(path, target)
	}
	if err != nil {
		return problem("storage", "publish-failed", "Private state could not be published; retain the partial receipt.")
	}
	dir, err := os.Open(root) //nolint:gosec // fsync the already validated owned private state directory
	if err != nil {
		return problem("storage", "sync-failed", "Private state directory could not be synced.")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return problem("storage", "sync-failed", "Private state directory could not be synced.")
	}
	return nil
}

func validReceipt(r Receipt, root string) bool {
	if !validOperation(r.OperationID) || !validTarget(r.Target) || r.Authority == "" || environment.ValidateAuthority(r.Authority) != nil || service.ValidateVersion(r.Version) != nil || r.RemotePort < 1024 || r.RemotePort > 65535 {
		return false
	}
	switch r.Phase {
	case "preflight", "installing", "installed", "exchange-uncertain", "paired", "complete", "rolled-back":
	default:
		return false
	}
	if r.ArchiveSHA256 != "" && !digestPattern.MatchString(r.ArchiveSHA256) {
		return false
	}
	if _, err := identity.NormalizeDeviceScopes(r.Scopes); err != nil {
		return false
	}
	if !reflect.DeepEqual(r.Scopes, normalizedScopes(r.Scopes)) {
		return false
	}
	if r.EnvironmentID != "" && !validOperation(r.EnvironmentID) {
		return false
	}
	if r.DeviceID != "" && (!strings.HasPrefix(r.DeviceID, "msg://device/") || !validOpaque("tth_"+strings.TrimPrefix(r.DeviceID, "msg://device/"), "tth_")) {
		return false
	}
	if r.GrantID != "" && (!strings.HasPrefix(r.GrantID, "grant_") || !validOpaque("tth_"+strings.TrimPrefix(r.GrantID, "grant_"), "tth_")) {
		return false
	}
	if r.CredentialReference != "" {
		u, e := url.Parse(r.CredentialReference)
		if e != nil || u.Scheme != "file" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != filepath.Join(root, "device.token") {
			return false
		}
	}
	switch r.Phase {
	case "installed", "exchange-uncertain", "paired", "complete":
		if r.EnvironmentID == "" || r.ArchiveSHA256 == "" {
			return false
		}
	}
	if (r.Phase == "paired" || r.Phase == "complete") && (r.DeviceID == "" || r.GrantID == "" || r.CredentialReference == "") {
		return false
	}
	return true
}
