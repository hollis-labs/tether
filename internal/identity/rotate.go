package identity

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// RotateResult describes a completed operator rotation without carrying any
// credential. The hash prefixes identify the credentials in audit output; a
// prefix of a SHA-256 over a 256-bit random token reveals nothing usable.
type RotateResult struct {
	// OldHashPrefix is the first 12 hex chars of the hash of the credential the
	// token file held, or "" when the file was missing or did not verify.
	OldHashPrefix string
	NewHashPrefix string
	// Revoked is how many active operator credentials were revoked.
	Revoked int64
	// OldFileValid reports whether the token file held an active operator
	// credential before the rotation (false is the recovery case).
	OldFileValid bool
}

// IsBoundAgentToken reports whether p is an issued session-bound agent token
// (a JWT minted with a session_id, Kind agent): TokenID marks it as issued,
// SessionID names the session and the subject must be that session's principal
// id. It deliberately keys on these, never on Kind alone, so an operator-minted
// narrow token (Kind operator, TokenID set) can never pass as a session.
func IsBoundAgentToken(p Principal) bool {
	return p.Kind == "agent" && p.Verified && p.TokenID != "" && p.SessionID != "" && p.ID == SessionPrincipalIDPrefix+p.SessionID
}

// SessionPrincipalIDPrefix is the subject prefix of an agent session's principal
// (authissuer.SessionPrincipalID builds the same value).
const SessionPrincipalIDPrefix = "msg://session/local/"

// rotateAfterReplace is a test seam: it runs after the new token file has
// replaced the old one and before the database transaction commits.
var rotateAfterReplace func() error

// ErrOperatorRotationBusy is returned when the state database stays locked.
var ErrOperatorRotationBusy = errors.New("state database is locked by another writer; retry, or stop the daemon first")

// RotateOperator replaces the operator credential: it revokes every active
// operator credential, mints a new one and atomically replaces the token file
// at path. The daemon verifies each request against the database, so a running
// daemon accepts the new credential and rejects the old one immediately.
//
// Failure leaves the old pair valid. The database change is one transaction
// that is committed last; the file is replaced by renaming a fully written
// temporary file over the old one, and the old file stays reachable as a hard
// link until the commit succeeds, so a failed commit restores it. The one
// uncovered window is a crash between the rename and the commit: the file then
// holds a token the database has not seen, and running this again repairs it
// (it does not require the old file to verify).
func (s *Store) RotateOperator(ctx context.Context, path string) (RotateResult, error) {
	var res RotateResult
	if s == nil || s.db == nil {
		return res, fmt.Errorf("identity store unavailable")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return res, fmt.Errorf("create token directory: %w", err)
	}
	oldExists := false
	if fi, err := os.Lstat(path); err == nil {
		if !fi.Mode().IsRegular() {
			return res, fmt.Errorf("operator token path is not a regular file; refusing to replace it")
		}
		oldExists = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return res, fmt.Errorf("inspect operator token file: %w", err)
	}
	if oldExists {
		if old, err := ReadTokenFile(path); err == nil {
			if p, verr := s.Verify(ctx, old); verr == nil && p.ID == OperatorID && p.Kind == "operator" {
				res.OldFileValid = true
				res.OldHashPrefix = HashToken(old)[:12]
			}
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return res, fmt.Errorf("begin rotation: %w", rotationErr(err))
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	revoked, err := tx.ExecContext(ctx, `UPDATE principals SET revoked_at = ? WHERE principal_id = ? AND revoked_at IS NULL`, time.Now().UTC().Format(time.RFC3339Nano), OperatorID)
	if err != nil {
		return res, fmt.Errorf("revoke operator credential: %w", rotationErr(err))
	}
	res.Revoked, _ = revoked.RowsAffected()
	token, err := mint(ctx, tx, Principal{ID: OperatorID, Kind: "operator", Display: "Local operator", Scopes: []string{"*"}, Addresses: []string{OperatorID}})
	if err != nil {
		return res, fmt.Errorf("mint operator credential: %w", rotationErr(err))
	}
	res.NewHashPrefix = HashToken(token)[:12]

	tmp, err := writeTempTokenFile(dir, token)
	if err != nil {
		return res, err
	}
	defer func() { _ = os.Remove(tmp) }() // no-op once renamed into place

	prev := path + ".prev"
	hadPrev := false
	if oldExists {
		_ = os.Remove(prev)
		if err := os.Link(path, prev); err != nil {
			return res, fmt.Errorf("keep the old operator token file: %w", err)
		}
		hadPrev = true
	}
	if err := os.Rename(tmp, path); err != nil {
		if hadPrev {
			_ = os.Remove(prev)
		}
		return res, fmt.Errorf("replace operator token file: %w", err)
	}
	restore := func() {
		if hadPrev {
			_ = os.Rename(prev, path)
		} else {
			_ = os.Remove(path)
		}
	}
	if rotateAfterReplace != nil {
		if err := rotateAfterReplace(); err != nil {
			restore()
			return res, err
		}
	}
	if err := tx.Commit(); err != nil {
		restore()
		return res, fmt.Errorf("commit rotation: %w", rotationErr(err))
	}
	committed = true
	if hadPrev {
		_ = os.Remove(prev)
	}
	if d, err := os.Open(dir); err == nil { //nolint:gosec // G304: run dir beside the catalog
		_ = d.Sync()
		_ = d.Close()
	}
	return res, nil
}

func rotationErr(err error) error {
	if err != nil && (strings.Contains(err.Error(), "database is locked") || strings.Contains(err.Error(), "SQLITE_BUSY")) {
		return fmt.Errorf("%w: %w", ErrOperatorRotationBusy, err)
	}
	return err
}

// writeTempTokenFile writes token to a new 0600 file in dir and syncs it.
func writeTempTokenFile(dir, token string) (string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("temp token name: %w", err)
	}
	name := filepath.Join(dir, ".operator.token.new-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: run dir beside the catalog; exclusive creation
	if err != nil {
		return "", fmt.Errorf("create temp token file: %w", err)
	}
	if _, err := f.WriteString(token + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("write temp token file: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("sync temp token file: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("close temp token file: %w", err)
	}
	return name, nil
}
