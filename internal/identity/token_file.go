package identity

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// ReadTokenFile never follows a symlink or blocks on a special file. It checks
// the opened inode, rather than trusting a pathname check before open.
func ReadTokenFile(path string) (string, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: operator-selected path; opened inode must be owned, regular and 0600.
	if err != nil {
		return "", fmt.Errorf("open token file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat token file: %w", err)
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || !ok || int(owner.Uid) != os.Getuid() {
		return "", fmt.Errorf("token file must be a regular file owned by this user with mode 0600")
	}
	body, err := io.ReadAll(io.LimitReader(f, 257))
	if err != nil {
		return "", fmt.Errorf("read token file: %w", err)
	}
	token := strings.TrimSpace(string(body))
	if len(body) > 256 || !validToken(token) {
		return "", fmt.Errorf("token file has invalid content")
	}
	return token, nil
}

// WriteTokenFile is exclusive: it never overwrites an existing credential.
func WriteTokenFile(path, token string) error {
	if !validToken(token) {
		return ErrInvalidToken
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create token directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: daemon-selected credential path; exclusive creation cannot follow/overwrite an existing symlink.
	if err != nil {
		return fmt.Errorf("create token file: %w", err)
	}
	// Keep a failed, incomplete file in place so the next startup fails closed.
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		return fmt.Errorf("write token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("sync token file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close token file: %w", err)
	}
	return nil
}

// EnsureOperator is called only at daemon startup, never by a client. An
// existing file must already verify against this database; it is not adopted
// or repaired implicitly, and a missing file cannot rotate an existing token.
func (s *Store) EnsureOperator(ctx context.Context, path string) error {
	token, err := ReadTokenFile(path)
	if err == nil {
		p, err := s.Verify(ctx, token)
		if err != nil {
			return fmt.Errorf("operator credential does not verify: %w", err)
		}
		if p.ID != OperatorID || p.Kind != "operator" {
			return fmt.Errorf("operator file contains another principal's credential")
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	token, err = s.Mint(ctx, Principal{ID: OperatorID, Kind: "operator", Display: "Local operator", Scopes: []string{"*"}, Addresses: []string{OperatorID}})
	if err != nil {
		return fmt.Errorf("bootstrap operator: %w", err)
	}
	if err := WriteTokenFile(path, token); err != nil {
		_ = s.Revoke(ctx, OperatorID)
		return err
	}
	return nil
}
