//go:build linux

package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimretire"
)

// This private constructor is test-only. It binds SYNTHETIC immutable authority
// and full containment to owned fixtures, never a production success switch.
// SQL, native time, writer reservation and unlink use the actual kernel.
func fixtureCleanupAdmission(t *testing.T, s *Store, root *os.Root, receipt shimretire.Receipt, cursor *shimretire.SweepCursor) *ShimCleanupAdmission {
	t.Helper()
	var seq int
	var schema, name string
	if err := s.db.QueryRow("PRAGMA database_list").Scan(&seq, &schema, &name); err != nil {
		t.Fatal(err)
	}
	if seq != 0 || schema != "main" || !filepath.IsAbs(name) {
		t.Fatal("fixture database identity unavailable")
	}
	// A dedicated zero-busy connection bounds BEGIN to one immediate attempt;
	// it does not alter the shared Store connection or any global settings.
	u := url.URL{Scheme: "file", Path: name}
	db, err := sql.Open("sqlite", u.String()+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	if err = db.PingContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := os.Chmod(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(filepath.Dir(name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dir.Close() })
	ri, err := root.Lstat(".")
	if err != nil {
		t.Fatal(err)
	}
	c := &cleanupCustody{gate: make(chan struct{}, 1), root: root, rootIdentity: nativeCleanupIdentity(ri), request: receipt.Request, proof: receipt.Clone().Proof, directory: dir, databasePath: name, namespace: make(map[string]*os.File)}
	artifact := receipt.Snapshot.Descriptor
	if cursor != nil && cursor.Pending != nil {
		artifact = cursor.Pending.Artifact
	}
	c.rootID, c.ownerID, c.custodyRevision = artifact.RootID, artifact.OwnerID, artifact.CustodyRevision
	c.gate <- struct{}{}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		p := name + suffix
		if err = os.Chmod(p, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		c.namespace[p] = f
		if suffix == "" {
			c.database = f
		}
	}
	if cursor != nil {
		r := cursor.Request.Clone()
		c.sweep = &r
	}
	if !c.namedDatabase() {
		t.Fatal("fixture native database namespace custody unavailable")
	}
	return &ShimCleanupAdmission{db: db, custody: c}
}

func TestCleanupAdmissionRetainsUnprovenQuotaAndItemBudget(t *testing.T) {
	for _, kind := range []string{"quota_only", "quota_age_floor", "item_budget"} {
		t.Run(kind, func(t *testing.T) {
			s, _, attempt := nativeAdmissionFixture(t)
			c := attempt.Cursor.Clone()
			c.Request.OperationID = kind
			c.ID = kind
			c.Revision = ""
			c.Phase = shimretire.CursorReady
			c.Pending = nil
			if kind == "item_budget" {
				c.Request.Budget.MaxBytesRemoved = 1
			} else {
				quotaLimit := uint64(1)
				c.Request.Policy.MaxRetainedBytes = &quotaLimit
				if kind == "quota_only" {
					c.Request.Policy.MinRetiredAge = nil
				}
			}
			raw, _ := json.Marshal(c.Request)
			digest := sha256.Sum256(raw)
			c.RequestDigest = hex.EncodeToString(digest[:])
			adapter := ShimRetentionStore{Store: s}
			var err error
			c.Revision, err = adapter.RecordCursor(context.Background(), c, "")
			if err != nil {
				t.Fatal(err)
			}
			c.Pending = attempt.Cursor.Clone().Pending
			c.Phase = shimretire.CursorIntent
			c.Revision, err = adapter.RecordCursor(context.Background(), c, c.Revision)
			if err != nil {
				t.Fatal(err)
			}
			a := fixtureCleanupAdmission(t, s, attempt.Root, attempt.Receipt, &c)
			attempt.Cursor = &c
			m, err := a.RemoveOwned(context.Background(), attempt)
			payload, readErr := attempt.Root.ReadFile(attempt.Artifact.RelativePath)
			if m != shimretire.NoChange || err == nil || readErr != nil || string(payload) != "private" {
				t.Fatalf("unproven policy removed data: %s %v %q %v", m, err, payload, readErr)
			}
			saved, e := adapter.LoadCursor(context.Background(), c.ID)
			if e != nil || saved.Revision != c.Revision || saved.Phase != shimretire.CursorIntent {
				t.Fatal("refusal lost actual durable pending cursor")
			}
		})
	}
}

func TestNativeAdmissionClockAdvances(t *testing.T) {
	c := newNativeAdmissionClock()
	before := c.now()
	time.Sleep(3 * time.Millisecond)
	after := c.now()
	if !after.After(before) || after.Sub(c.anchor.UTC()) < 3*time.Millisecond {
		t.Fatal("native time froze original proof budget")
	}
}

func nativeAdmissionFixture(t *testing.T) (*Store, *ShimCleanupAdmission, shimretire.CleanupAttempt) {
	t.Helper()
	s := retirementDB(t)
	r := retirementReceipt()
	r.Proof.ObservedAt = time.Now().UTC()
	r.Proof.ValidUntil = r.Proof.ObservedAt.Add(time.Minute)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	for _, name := range []string{"launch.json", "host.log"} {
		if err = root.WriteFile(name, []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	descriptor, err := root.Lstat("launch.json")
	if err != nil {
		t.Fatal(err)
	}
	r.Snapshot.Descriptor.Identity = nativeCleanupIdentity(descriptor)
	r.Snapshot.Descriptor.Size = 7
	artifact := r.Snapshot.Descriptor
	artifact.Category = shimretire.HostLog
	artifact.RelativePath = "host.log"
	payload, err := root.Lstat("host.log")
	if err != nil {
		t.Fatal(err)
	}
	artifact.Identity = nativeCleanupIdentity(payload)
	r.Inventory = []shimretire.Artifact{r.Snapshot.Descriptor, artifact}
	if err = s.CreateSession(SessionRow{ID: r.Request.Placement.Session, State: "orphaned"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	adapter := ShimRetirementStore{Store: s}
	for _, phase := range []shimretire.Phase{shimretire.IntentRecorded, shimretire.RetirementCommitted, shimretire.DescriptorCleanupComplete, shimretire.StateReconciled} {
		r.Phase = phase
		if phase != shimretire.IntentRecorded {
			r.Snapshot.Retired = true
			r.RetiredAt = r.Proof.ObservedAt.Add(-time.Hour)
		}
		r.Revision, err = adapter.Record(context.Background(), r, r.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	c := retentionCursorFixture()
	cursorAdapter := ShimRetentionStore{Store: s}
	c.Revision, err = cursorAdapter.RecordCursor(context.Background(), c, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Phase = shimretire.CursorIntent
	c.Pending = &shimretire.SweepCandidate{RetirementOperation: r.OperationID, Artifact: artifact, SortKey: "retired/session/host.log", Hold: shimretire.NoRetentionHold, HoldRevision: "holds"}
	c.Revision, err = cursorAdapter.RecordCursor(context.Background(), c, c.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// Use the actual durable JSON representation, not fixture-only locations or
	// monotonic time metadata that do not survive receipt persistence.
	r, err = adapter.Load(context.Background(), r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	c, err = cursorAdapter.LoadCursor(context.Background(), c.ID)
	if err != nil {
		t.Fatal(err)
	}
	a := fixtureCleanupAdmission(t, s, root, r, &c)
	return s, a, shimretire.CleanupAttempt{Mode: shimretire.RetentionCleanup, Receipt: r, Snapshot: r.Snapshot, Proof: r.Proof, Artifact: artifact, Cursor: &c, Root: root, RootIdentity: a.custody.rootIdentity}
}

func TestCleanupAdmissionProductionAndZeroConstruction(t *testing.T) {
	s, a, attempt := nativeAdmissionFixture(t)
	if supported, err := NewShimCleanupAdmission(s); supported != nil || !errors.Is(err, shimretire.ErrCleanupUnsupported) {
		t.Fatal("production custody was manufactured")
	}
	for _, foreign := range []*ShimCleanupAdmission{nil, {}, {db: a.db}} {
		m, err := foreign.RemoveOwned(context.Background(), attempt)
		if m != shimretire.NoChange || !errors.Is(err, shimretire.ErrCleanupUnsupported) {
			t.Fatalf("foreign constructor admitted: %s %v", m, err)
		}
	}
	b, err := attempt.Root.ReadFile(attempt.Artifact.RelativePath)
	if err != nil || string(b) != "private" {
		t.Fatal("unsupported construction changed payload")
	}
	// A genuine positive uses the same actual SQL/native-time/native-FS kernel.
	m, err := a.RemoveOwned(context.Background(), attempt)
	if m != shimretire.Changed || err != nil {
		t.Fatalf("native owned positive: %s %v", m, err)
	}
}

func TestCleanupAdmissionFencesActualAlternateWriters(t *testing.T) {
	for _, writer := range []string{"audit", "cursor", "session"} {
		t.Run(writer, func(t *testing.T) {
			s, a, attempt := nativeAdmissionFixture(t)
			other, err := sql.Open("sqlite", a.custody.databasePath+"?_pragma=busy_timeout(0)")
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.SetMaxOpenConns(1)
			otherStore := &Store{db: other}
			write := func() error {
				switch writer {
				case "audit":
					r := attempt.Receipt.Clone()
					r.Obligations = []string{"cleanup_pending"}
					_, e := (ShimRetirementStore{Store: otherStore}).Record(context.Background(), r, r.Revision)
					return e
				case "cursor":
					c := attempt.Cursor.Clone()
					_, e := (ShimRetentionStore{Store: otherStore}).RecordCursor(context.Background(), c, c.Revision)
					return e
				default:
					result, e := other.Exec("UPDATE sessions SET updated_at='fixture' WHERE id=?", attempt.Receipt.Request.Placement.Session)
					if e != nil {
						return e
					}
					n, e := result.RowsAffected()
					if n != 1 {
						return errors.New("session writer fixture missing")
					}
					return e
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			conn, err := a.reserveAdmission(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if err = write(); err == nil {
				_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
				_ = conn.Close()
				t.Fatal("actual alternate writer crossed native reservation")
			}
			current, err := admissionReceipt(ctx, conn, attempt.Receipt.OperationID)
			if err != nil || !sameCleanupReceipt(current, attempt.Receipt) {
				t.Fatal("guarded audit changed")
			}
			cursor, err := admissionCursor(ctx, conn, attempt.Cursor.ID)
			if err != nil || cursor.Revision != attempt.Cursor.Revision {
				t.Fatal("guarded cursor changed")
			}
			if _, err = conn.ExecContext(ctx, "ROLLBACK"); err != nil {
				t.Fatal(err)
			}
			if err = conn.Close(); err != nil {
				t.Fatal(err)
			}
			if err = write(); err != nil {
				t.Fatalf("otherwise valid writer failed after reservation release: %v", err)
			}
			// The reservation is a writer fence, not a second durable authority sink.
			var count int
			if err = s.db.QueryRow("SELECT count(*) FROM session_shim_retirements").Scan(&count); err != nil || count != 1 {
				t.Fatal("reservation changed audit inventory")
			}
		})
	}
}
