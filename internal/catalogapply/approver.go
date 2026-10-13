package catalogapply

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/crypto/scrypt"
)

// Approver verifies the operator's approval passphrase. The passphrase is a
// secret a human types; it is what separates an approval from anything a
// launched agent can do with the same OS user, the same socket and the same
// operator token file. Only a scrypt hash is stored.
//
// The guarantee, and where it stops: an agent that does not know the
// passphrase cannot approve a plan, whatever it can reach. An attacker with
// the operator's keystrokes (a ptrace or keylogger on the operator's own
// terminal), or one who can rewrite this file directly, is outside it; so is
// anyone who can write the catalog files directly, which needs no approval.
type Approver struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	failures int
	lockedTo time.Time
}

// ErrNoPassphrase means no approval passphrase is configured, so approval is
// refused rather than defaulted open.
var ErrNoPassphrase = errors.New("no approval passphrase is configured; run `tether catalog approver set` on a terminal")

// ErrBadPassphrase is a wrong passphrase.
var ErrBadPassphrase = errors.New("approval passphrase is wrong")

// ErrLockedOut is returned after repeated wrong passphrases.
var ErrLockedOut = errors.New("approval is locked out after repeated wrong passphrases; wait and retry")

// scryptN is the scrypt cost for new hashes; tests lower it.
var scryptN = 1 << 15

const (
	minPassphraseLen = 12
	maxFailures      = 5
	lockoutDuration  = 5 * time.Minute
)

type approverFile struct {
	Salt string `json:"salt"`
	Hash string `json:"hash"`
	N    int    `json:"n"`
	R    int    `json:"r"`
	P    int    `json:"p"`
}

// NewApprover keeps the passphrase hash at path (mode 0600).
func NewApprover(path string) *Approver {
	return &Approver{path: path, now: time.Now}
}

func (a *Approver) load() (approverFile, error) {
	var f approverFile
	b, err := os.ReadFile(a.path)
	if errors.Is(err, os.ErrNotExist) {
		return f, ErrNoPassphrase
	}
	if err != nil {
		return f, err
	}
	if err := json.Unmarshal(b, &f); err != nil || f.Hash == "" || f.Salt == "" {
		return f, fmt.Errorf("approver file %s is unreadable", a.path)
	}
	return f, nil
}

func derive(pass string, f approverFile) ([]byte, error) {
	salt, err := base64.StdEncoding.DecodeString(f.Salt)
	if err != nil {
		return nil, err
	}
	return scrypt.Key([]byte(pass), salt, f.N, f.R, f.P, 32)
}

// Configured reports whether a passphrase is set.
func (a *Approver) Configured() bool {
	_, err := a.load()
	return err == nil
}

// Set installs a new passphrase. When one is already configured, current must
// match it.
func (a *Approver) Set(current, next string) error {
	if len(next) < minPassphraseLen {
		return fmt.Errorf("passphrase must be at least %d characters", minPassphraseLen)
	}
	if a.Configured() {
		if err := a.Verify(current); err != nil {
			return err
		}
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return err
	}
	f := approverFile{Salt: base64.StdEncoding.EncodeToString(salt), N: scryptN, R: 8, P: 1}
	h, err := derive(next, f)
	if err != nil {
		return err
	}
	f.Hash = base64.StdEncoding.EncodeToString(h)
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	tmp := a.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, a.path)
}

// Verify checks pass against the stored hash in constant time, with a
// lockout after repeated failures.
func (a *Approver) Verify(pass string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if now := a.now(); now.Before(a.lockedTo) {
		return ErrLockedOut
	}
	f, err := a.load()
	if err != nil {
		return err
	}
	got, err := derive(pass, f)
	if err != nil {
		return err
	}
	want, err := base64.StdEncoding.DecodeString(f.Hash)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		a.failures++
		if a.failures >= maxFailures {
			a.lockedTo = a.now().Add(lockoutDuration)
			a.failures = 0
		}
		return ErrBadPassphrase
	}
	a.failures = 0
	return nil
}
