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
func TestConfinedDescriptorRequiresDurableRetirement(t *testing.T) {
	for _, phase := range []Phase{IntentRecorded, RetirementCommitted} {
		t.Run(string(phase), func(t *testing.T) {
			c, a, store, name := cleanupFixture(t)
			store.receipt.Phase = phase
			if phase == IntentRecorded {
				store.receipt.Snapshot.Retired = false
				store.receipt.RetiredAt = time.Time{}
			}
			mutation, err := c.Descriptor(context.Background(), "retirement", a)
			if phase == IntentRecorded {
				if err == nil || mutation != NoChange {
					t.Fatalf("intent permits cleanup: %s %v", mutation, err)
				}
				if _, err := os.Stat(name); err != nil {
					t.Fatal(err)
				}
			} else {
				if err != nil || mutation != Changed {
					t.Fatalf("cleanup: %s %v", mutation, err)
				}
				if _, err := os.Stat(name); !os.IsNotExist(err) {
					t.Fatalf("descriptor retained: %v", err)
				}
				if m, e := c.Descriptor(context.Background(), "retirement", a); m != NoChange || e != nil {
					t.Fatalf("retry: %s %v", m, e)
				}
			}
		})
	}
}
func TestConfinedDescriptorObservesAfterAuthority(t *testing.T) {
	c, a, _, name := cleanupFixture(t)
	changed := false
	c.Validate = func(context.Context, Request) error {
		if changed {
			return nil
		}
		changed = true
		if err := os.Rename(name, name+".old"); err != nil {
			return err
		}
		return os.WriteFile(name, []byte("changed"), 0600)
	}
	m, err := c.Descriptor(context.Background(), "retirement", a)
	if err == nil || m != NoChange {
		t.Fatalf("replacement removed: %s %v", m, err)
	}
	b, err := os.ReadFile(name)
	if err != nil || string(b) != "changed" {
		t.Fatalf("replacement bytes: %q %v", b, err)
	}
}

func TestConfinedDescriptorRetainsLateUnknownProof(t *testing.T) {
	c, a, store, name := cleanupFixture(t)
	c.Validate = func(context.Context, Request) error { store.receipt.Proof.Descendants = ExecutionUnknown; return nil }
	m, err := c.Descriptor(context.Background(), "retirement", a)
	if err == nil || m != NoChange {
		t.Fatalf("late unknown cleanup: %s %v", m, err)
	}
	if _, err = os.Stat(name); err != nil {
		t.Fatal(err)
	}
}

func TestConfinedDescriptorRefusesLostRootAndHardlink(t *testing.T) {
	for _, kind := range []string{"renamed-root", "replaced-root", "hardlink", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			c, a, _, name := cleanupFixture(t)
			changed := false
			c.Validate = func(context.Context, Request) error {
				if changed {
					return nil
				}
				changed = true
				switch kind {
				case "renamed-root", "replaced-root":
					if err := os.Rename(filepath.Dir(name), filepath.Dir(name)+".moved"); err != nil {
						return err
					}
					if kind == "replaced-root" {
						return os.Mkdir(filepath.Dir(name), 0700)
					}
					return nil
				case "hardlink":
					return os.Link(name, name+".alias")
				default:
					if err := os.Rename(name, name+".old"); err != nil {
						return err
					}
					return os.Symlink(name+".old", name)
				}
			}
			if kind == "renamed-root" || kind == "replaced-root" {
				t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(name) + ".moved") })
			}
			m, err := c.Descriptor(context.Background(), "retirement", a)
			if err == nil || m != NoChange {
				t.Fatalf("%s custody accepted: %s %v", kind, m, err)
			}
			retained := name
			if kind == "renamed-root" || kind == "replaced-root" {
				retained = filepath.Join(filepath.Dir(name)+".moved", filepath.Base(name))
			}
			b, err := os.ReadFile(retained)
			if err != nil || string(b) != "private" {
				t.Fatalf("%s bytes lost: %q %v", kind, b, err)
			}
		})
	}
}

func TestConfinedRetentionRequiresDurableIntentAndFreshHold(t *testing.T) {
	for _, kind := range []string{"valid", "no-intent", "late-hold", "late-intent", "age-floor"} {
		t.Run(kind, func(t *testing.T) {
			cleanup, descriptor, audit, _ := cleanupFixture(t)
			f, err := cleanup.Root.OpenFile("host.log", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = f.Write([]byte("private")); err != nil {
				t.Fatal(err)
			}
			info, err := f.Stat()
			if err != nil {
				t.Fatal(err)
			}
			if err = f.Close(); err != nil {
				t.Fatal(err)
			}
			artifact := descriptor
			artifact.RelativePath = "host.log"
			artifact.Category = HostLog
			artifact.Identity = fileIdentity(info)
			artifact.Size = 7
			if _, err = cleanup.Descriptor(context.Background(), "retirement", descriptor); err != nil {
				t.Fatal(err)
			}
			audit.receipt.Phase = StateReconciled
			audit.receipt.Inventory = []Artifact{descriptor, artifact}
			audit.receipt.RetiredAt = cleanup.Now().Add(-time.Hour)
			request, _, _, _, _ := retentionFixture()
			request.Policy.RootIDs = []string{artifact.RootID}
			candidate := SweepCandidate{RetirementOperation: "retirement", Artifact: artifact, SortKey: "retired/session/host.log", Hold: NoRetentionHold, HoldRevision: "holds"}
			cursors := &memoryCursors{cursor: SweepCursor{Version: RetentionVersion, ID: request.OperationID, Request: request, RequestDigest: retentionDigest(request), Revision: "cursor", InventoryRevision: "inventory", Phase: CursorIntent, Pending: &candidate}}
			removal := RetentionRemoval{Cleanup: cleanup, Request: request, Cursors: cursors, Validate: func(context.Context, SweepRequest) error { return nil }, Observe: func(context.Context, SweepCandidate) (SweepCandidate, Snapshot, Observation, error) {
				return candidate, audit.receipt.Snapshot, audit.receipt.Proof, nil
			}}
			switch kind {
			case "no-intent":
				cursors.cursor.Phase = CursorReady
				cursors.cursor.Pending = nil
			case "late-hold":
				removal.Validate = func(context.Context, SweepRequest) error { candidate.Hold = RetentionHeld; return nil }
			case "late-intent":
				removal.Validate = func(context.Context, SweepRequest) error {
					cursors.cursor.Pending.Artifact.Identity.Inode++
					return nil
				}
			case "age-floor":
				audit.receipt.RetiredAt = cleanup.Now()
			}
			original := candidate
			copyCandidate := candidate
			cursors.cursor.Pending = &copyCandidate
			mutation, err := removal.Remove(context.Background(), original)
			if kind == "valid" {
				if err != nil || mutation != Changed {
					t.Fatalf("retention: %s %v", mutation, err)
				}
			} else {
				if err == nil || mutation != NoChange {
					t.Fatalf("%s permits cleanup: %s %v", kind, mutation, err)
				}
				b, e := cleanup.Root.ReadFile("host.log")
				if e != nil || string(b) != "private" {
					t.Fatalf("%s private bytes lost: %q %v", kind, b, e)
				}
			}
		})
	}
}

func TestConfinedDescriptorRechecksAuthorityAfterObservation(t *testing.T) {
	c, a, store, name := cleanupFixture(t)
	revoked := false
	c.Validate = func(context.Context, Request) error {
		if revoked {
			return errors.New("revoked")
		}
		return nil
	}
	c.Observe = func(context.Context) (Snapshot, Observation, error) {
		revoked = true
		return store.receipt.Snapshot, store.receipt.Proof, nil
	}
	mutation, err := c.Descriptor(context.Background(), "retirement", a)
	if err == nil || mutation != NoChange {
		t.Fatalf("observation revocation ignored: %s %v", mutation, err)
	}
	if _, err = os.Stat(name); err != nil {
		t.Fatal(err)
	}
}
