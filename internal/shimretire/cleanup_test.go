//go:build linux || darwin

package shimretire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func cleanupFixture(t *testing.T) (ConfinedCleanup, Artifact, *memoryStore, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "launch.json")
	if err := os.WriteFile(name, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	info, err := root.Lstat("launch.json")
	if err != nil {
		t.Fatal(err)
	}
	ri, err := root.Lstat(".")
	if err != nil {
		t.Fatal(err)
	}
	r, s, o, now := validProof()
	s.Descriptor.Identity = fileIdentity(info)
	s.Descriptor.Size = uint64(info.Size())
	s.Retired = true
	events := []string{}
	store := &memoryStore{events: &events}
	rawReceipt := Receipt{Version: Version, OperationID: r.OperationID, Request: r, Snapshot: s, Proof: o, Phase: RetirementCommitted, RetiredAt: now, Revision: "revision"}
	encoded, _ := json.Marshal(r)
	digest := sha256.Sum256(encoded)
	rawReceipt.RequestDigest = hex.EncodeToString(digest[:])
	store.receipt = rawReceipt
	return ConfinedCleanup{Root: root, RootID: s.Descriptor.RootID, OwnerID: s.Descriptor.OwnerID, CustodyRevision: s.Descriptor.CustodyRevision, RootIdentity: fileIdentity(ri), Store: store, Validate: func(context.Context, Request) error { return nil }, Observe: func(context.Context) (Snapshot, Observation, error) {
		return store.receipt.Snapshot, store.receipt.Proof, nil
	}, Now: func() time.Time { return now }}, s.Descriptor, store, name
}

// Physical cleanup controls now live with the real SQL/native kernel in store.
// A neutral interface must never supply the effect capability.
func TestConfinedCleanupRejectsForeignAdmission(t *testing.T) {
	for _, kind := range []string{"nil", "foreign"} {
		t.Run(kind, func(t *testing.T) {
			c, a, _, name := cleanupFixture(t)
			foreign := &foreignCleanupAdmission{}
			if kind == "foreign" {
				c.Admission = foreign
			}
			m, err := c.Descriptor(context.Background(), "retirement", a)
			b, readErr := os.ReadFile(name)
			if m != NoChange || !errors.Is(err, ErrCleanupUnsupported) || foreign.called || readErr != nil || string(b) != "private" {
				t.Fatalf("foreign construction admitted: %s %v called=%t bytes=%q read=%v", m, err, foreign.called, b, readErr)
			}
		})
	}
}

type foreignCleanupAdmission struct{ called bool }

func (f *foreignCleanupAdmission) RemoveOwned(context.Context, CleanupAttempt) (Mutation, error) {
	f.called = true
	return Changed, nil
}
