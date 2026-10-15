// Package authissuer makes Tether the token issuer for the Hollis mesh: it
// owns the Ed25519 signing keys (with rotation overlap), mints scoped JWTs
// from an authenticated parent, publishes the JWKS and the deny-list, and
// verifies its own tokens for Tether's local API through go-auth.
package authissuer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// DefaultKeyOverlap is how long a rotated-out key stays in the JWKS: longer
// than the maximum token TTL plus verifier leeway and refresh interval, so no
// live token is ever signed by a key a verifier has dropped.
const DefaultKeyOverlap = 2 * time.Hour

type keyMeta struct {
	KID     string    `json:"kid"`
	Created time.Time `json:"created"`
	// RetireAt is zero for the active signing key; a retired key verifies
	// until RetireAt and then is deleted.
	RetireAt time.Time `json:"retire_at,omitzero"`
}

type keyRec struct {
	keyMeta
	priv ed25519.PrivateKey
}

// KeyStore manages signing keys under one 0700 directory: keys.json (index),
// <kid>.key (base64 private seed, 0600) and jwks.json (public, for
// BootstrapKeyFile consumers).
type KeyStore struct {
	dir     string
	overlap time.Duration
	now     func() time.Time

	mu   sync.Mutex
	keys []keyRec // newest first
}

// OpenKeyStore loads dir, creating it and a first key when absent. It refuses
// key files that are not mode 0600.
func OpenKeyStore(dir string, overlap time.Duration) (*KeyStore, error) {
	if overlap <= 0 {
		overlap = DefaultKeyOverlap
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // G302: 0700 is a directory mode
		return nil, err
	}
	ks := &KeyStore{dir: dir, overlap: overlap, now: time.Now}
	b, err := os.ReadFile(filepath.Join(dir, "keys.json")) //nolint:gosec // G304: dir is the issuer's own state directory
	switch {
	case err == nil:
		var metas []keyMeta
		if err := json.Unmarshal(b, &metas); err != nil {
			return nil, fmt.Errorf("authissuer: parse keys.json: %w", err)
		}
		for _, m := range metas {
			seed, err := readSeed(filepath.Join(dir, m.KID+".key"))
			if err != nil {
				return nil, err
			}
			ks.keys = append(ks.keys, keyRec{m, ed25519.NewKeyFromSeed(seed)})
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if ks.activeLocked() == nil {
		if err := ks.generateLocked(); err != nil {
			return nil, err
		}
	}
	return ks, ks.persistLocked()
}

func readSeed(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("authissuer: signing key %s must be a regular file with mode 0600", filepath.Base(path))
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path under the issuer's state directory
	if err != nil {
		return nil, err
	}
	seed, err := base64.RawStdEncoding.DecodeString(string(trimNL(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("authissuer: signing key %s is malformed", filepath.Base(path))
	}
	return seed, nil
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}

func (ks *KeyStore) activeLocked() *keyRec {
	for i := range ks.keys {
		if ks.keys[i].RetireAt.IsZero() {
			return &ks.keys[i]
		}
	}
	return nil
}

func (ks *KeyStore) generateLocked() error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(pub)
	kid := hex.EncodeToString(sum[:8])
	rec := keyRec{keyMeta{KID: kid, Created: ks.now().UTC()}, priv}
	if err := writeFile0600(filepath.Join(ks.dir, kid+".key"), []byte(base64.RawStdEncoding.EncodeToString(priv.Seed())+"\n")); err != nil {
		return err
	}
	ks.keys = append([]keyRec{rec}, ks.keys...)
	return nil
}

// Rotate retires the active key (it keeps verifying for the overlap window)
// and makes a new key active.
func (ks *KeyStore) Rotate() error {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if a := ks.activeLocked(); a != nil {
		a.RetireAt = ks.now().UTC().Add(ks.overlap)
	}
	if err := ks.generateLocked(); err != nil {
		return err
	}
	return ks.persistLocked()
}

// Prune deletes keys past their overlap window and reports whether any went.
func (ks *KeyStore) Prune() (bool, error) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	now := ks.now()
	kept := ks.keys[:0]
	removed := false
	for _, k := range ks.keys {
		if !k.RetireAt.IsZero() && !now.Before(k.RetireAt) {
			_ = os.Remove(filepath.Join(ks.dir, k.KID+".key"))
			removed = true
			continue
		}
		kept = append(kept, k)
	}
	ks.keys = kept
	if !removed {
		return false, nil
	}
	return true, ks.persistLocked()
}

// ActiveAge is how long the active key has been signing.
func (ks *KeyStore) ActiveAge() time.Duration {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	if a := ks.activeLocked(); a != nil {
		return ks.now().Sub(a.Created)
	}
	return 0
}

// Active returns the current signing key id and key.
func (ks *KeyStore) Active() (string, ed25519.PrivateKey) {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	a := ks.activeLocked()
	return a.KID, a.priv
}

// KIDs lists the key ids currently published (active first).
func (ks *KeyStore) KIDs() []string {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	out := make([]string, 0, len(ks.keys))
	for _, k := range ks.keys {
		out = append(out, k.KID)
	}
	return out
}

// JWKS renders the public key set: every key not yet past its overlap window.
func (ks *KeyStore) JWKS() []byte {
	ks.mu.Lock()
	defer ks.mu.Unlock()
	return ks.jwksLocked()
}

func (ks *KeyStore) jwksLocked() []byte {
	type jwk struct {
		Kty string `json:"kty"`
		Crv string `json:"crv"`
		Kid string `json:"kid"`
		Use string `json:"use"`
		Alg string `json:"alg"`
		X   string `json:"x"`
	}
	doc := struct {
		Keys []jwk `json:"keys"`
	}{Keys: []jwk{}}
	now := ks.now()
	for _, k := range ks.keys {
		if !k.RetireAt.IsZero() && !now.Before(k.RetireAt) {
			continue
		}
		doc.Keys = append(doc.Keys, jwk{"OKP", "Ed25519", k.KID, "sig", "EdDSA", base64.RawURLEncoding.EncodeToString(k.priv.Public().(ed25519.PublicKey))})
	}
	b, _ := json.Marshal(doc)
	return b
}

func (ks *KeyStore) persistLocked() error {
	sort.SliceStable(ks.keys, func(i, j int) bool { return ks.keys[i].Created.After(ks.keys[j].Created) })
	metas := make([]keyMeta, len(ks.keys))
	for i, k := range ks.keys {
		metas[i] = k.keyMeta
	}
	b, err := json.MarshalIndent(metas, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile0600(filepath.Join(ks.dir, "keys.json"), b); err != nil {
		return err
	}
	return writeFile0600(filepath.Join(ks.dir, "jwks.json"), ks.jwksLocked())
}

// writeFile0600 writes atomically: a 0600 temp file in the same directory,
// fsync, rename.
func writeFile0600(path string, b []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
