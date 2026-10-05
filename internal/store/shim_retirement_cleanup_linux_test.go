//go:build linux

package store

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimretire"
)

func TestRetirementCleanupFinalAudit(t *testing.T) {
	for _, category := range []shimretire.Category{shimretire.HostLog, shimretire.Descriptor} {
		for _, change := range []string{"pending", "revision", "unchanged", "read_error", "cancel", "validate_expiry", "read_expiry", "clock_audit", "clock_replace", "clock_cancel", "prior_pending", "missing", "renamed_root", "replaced_root", "hardlink", "symlink", "authority_revoke", "unknown_proof", "no_intent", "late_intent", "late_hold", "age_floor", "unretired", "db_replace", "wal_replace", "shm_replace", "native_wait_expiry", "native_guard_expiry", "sql_busy", "native_actual_expiry", "reminted_proof", "clock_cursor"} {
			if category == shimretire.Descriptor && (change == "no_intent" || change == "late_intent" || change == "late_hold" || change == "age_floor" || change == "clock_cursor") {
				continue // retention-only predicates; exercised by real HostLog
			}
			t.Run(string(category)+"/"+change, func(t *testing.T) {
				testRetirementCleanupFinalAudit(t, category, change)
			})
		}
	}
}

func testRetirementCleanupFinalAudit(t *testing.T, category shimretire.Category, change string) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	db := retirementDB(t)
	adapter := ShimRetirementStore{Store: db}
	r := retirementReceipt()
	r.Proof.ObservedAt = time.Now().UTC()
	r.Proof.ValidUntil = r.Proof.ObservedAt.Add(time.Minute)
	if change == "native_wait_expiry" || change == "native_guard_expiry" || change == "native_actual_expiry" {
		r.Proof.ValidUntil = r.Proof.ObservedAt.Add(400 * time.Millisecond)
	}
	if err := db.CreateSession(SessionRow{ID: r.Request.Placement.Session, State: "orphaned"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(dir, "host.log")
	if err := os.WriteFile(name, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	info, err := root.Lstat("host.log")
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)
	a := r.Snapshot.Descriptor
	a.Category = category
	a.RelativePath = "host.log"
	a.Size = 7
	a.Identity = shimretire.FileIdentity{Device: st.Dev, Inode: st.Ino, Kind: "regular"}
	if category == shimretire.Descriptor {
		r.Snapshot.Descriptor = a
	}
	r.Inventory = []shimretire.Artifact{a}
	if change == "prior_pending" {
		r.Obligations = []string{"cleanup_pending"}
	}
	for _, phase := range []shimretire.Phase{shimretire.IntentRecorded, shimretire.RetirementCommitted, shimretire.DescriptorCleanupComplete, shimretire.StateReconciled} {
		r.Phase = phase
		if phase != shimretire.IntentRecorded {
			r.Snapshot.Retired = true
			r.RetiredAt = r.Proof.ObservedAt.Add(-time.Hour)
		}
		if change == "age_floor" && phase != shimretire.IntentRecorded {
			r.RetiredAt = r.Proof.ObservedAt
		}
		rev, e := adapter.Record(ctx, r, r.Revision)
		if e != nil {
			t.Fatal(e)
		}
		r.Revision = rev
	}
	c := retentionCursorFixture()
	cursorStore := ShimRetentionStore{Store: db}
	c.Revision, err = cursorStore.RecordCursor(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	candidate := shimretire.SweepCandidate{RetirementOperation: r.OperationID, Artifact: a, SortKey: "retired/session/host.log", Hold: shimretire.NoRetentionHold, HoldRevision: "holds"}
	if category != shimretire.Descriptor {
		c.Phase = shimretire.CursorIntent
		c.Pending = &candidate
		c.Revision, err = cursorStore.RecordCursor(ctx, c, c.Revision)
		if err != nil {
			t.Fatal(err)
		}
	}
	ri, err := root.Lstat(".")
	if err != nil {
		t.Fatal(err)
	}
	rs := ri.Sys().(*syscall.Stat_t)
	calls := 0
	now := r.Proof.ObservedAt
	cleanup := shimretire.ConfinedCleanup{Root: root, RootID: a.RootID, OwnerID: a.OwnerID, CustodyRevision: a.CustodyRevision, RootIdentity: shimretire.FileIdentity{Device: rs.Dev, Inode: rs.Ino, Kind: "directory"}, Store: adapter, Now: func() time.Time { return now }, Validate: func(context.Context, shimretire.Request) error {
		calls++
		if calls == 2 && change == "validate_expiry" {
			now = r.Proof.ValidUntil
		}
		if calls == 2 && change == "authority_revoke" {
			return errors.New("retirement authority revoked")
		}
		if calls == 2 && (change == "pending" || change == "revision") {
			t.Logf("before final callback SQL CAS: revision=%s obligations=%v", r.Revision, r.Obligations)
			if change == "pending" {
				r.Obligations = []string{"cleanup_pending"}
			}
			rev, e := adapter.Record(ctx, r, r.Revision)
			if e != nil {
				return e
			}
			r.Revision = rev
			t.Logf("after final callback SQL CAS: revision=%s obligations=%v", r.Revision, r.Obligations)
		}
		return nil
	}}
	cleanup.Store = retirementAuditReadPort{Store: adapter, load: func(readCtx context.Context, operation string) (shimretire.Receipt, error) {
		current, err := adapter.Load(readCtx, operation)
		if calls == 2 && change == "read_expiry" {
			now = r.Proof.ValidUntil
		}
		if calls == 2 && change == "read_error" {
			return shimretire.Receipt{}, errors.New("final audit unavailable")
		}
		if calls == 2 && change == "cancel" {
			cancel()
		}
		return current, err
	}}
	var admittedCursor *shimretire.SweepCursor
	if category != shimretire.Descriptor {
		admittedCursor = &c
	}
	cleanup.Admission = fixtureCleanupAdmission(t, db, root, r, admittedCursor)
	if change == "missing" {
		if err = os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	if change == "unretired" {
		// A legitimate initial intent cannot authorize cleanup. Preserve the
		// completed fixture's raw audit while substituting only preflight evidence.
		old := cleanup.Store
		cleanup.Store = retirementAuditReadPort{Store: old, load: func(ctx context.Context, op string) (shimretire.Receipt, error) {
			v, e := old.Load(ctx, op)
			v.Snapshot.Retired = false
			v.Phase = shimretire.IntentRecorded
			v.RetiredAt = time.Time{}
			return v, e
		}}
	}
	if change == "native_wait_expiry" {
		kernel := cleanup.Admission.(*ShimCleanupAdmission)
		other, e := kernel.db.Conn(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = other.ExecContext(ctx, "BEGIN IMMEDIATE"); e != nil {
			t.Fatal(e)
		}
		defer func() { _, _ = other.ExecContext(context.Background(), "ROLLBACK"); _ = other.Close() }()
	}
	if change == "native_guard_expiry" {
		kernel := cleanup.Admission.(*ShimCleanupAdmission)
		<-kernel.custody.gate
		defer func() { kernel.custody.gate <- struct{}{} }()
	}
	if change == "sql_busy" {
		kernel := cleanup.Admission.(*ShimCleanupAdmission)
		otherDB, e := sql.Open("sqlite", kernel.custody.databasePath+"?_pragma=busy_timeout(0)")
		if e != nil {
			t.Fatal(e)
		}
		defer otherDB.Close()
		other, e := otherDB.Conn(ctx)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = other.ExecContext(ctx, "BEGIN IMMEDIATE"); e != nil {
			t.Fatal(e)
		}
		defer func() { _, _ = other.ExecContext(context.Background(), "ROLLBACK"); _ = other.Close() }()
	}
	if change == "native_actual_expiry" {
		time.Sleep(time.Until(r.Proof.ValidUntil) + 10*time.Millisecond)
	}
	clockCAS := false
	cleanup.Now = func() time.Time {
		if calls == 2 && !clockCAS && change != "clock_audit" {
			clockCAS = true
			switch change {
			case "clock_replace", "symlink":
				if e := os.Rename(name, name+".old"); e != nil {
					t.Fatal(e)
				}
				if change == "symlink" {
					if e := os.Symlink(name+".old", name); e != nil {
						t.Fatal(e)
					}
				} else if e := os.WriteFile(name, []byte("changed"), 0600); e != nil {
					t.Fatal(e)
				}
			case "clock_cursor":
				if category != shimretire.Descriptor {
					next := c.Clone()
					var e error
					next.Revision, e = cursorStore.RecordCursor(ctx, next, c.Revision)
					if e != nil {
						t.Fatal(e)
					}
				}
			case "clock_cancel":
				cancel()
			case "hardlink":
				if e := os.Link(name, name+".alias"); e != nil {
					t.Fatal(e)
				}
			case "renamed_root", "replaced_root":
				if e := os.Rename(dir, dir+".moved"); e != nil {
					t.Fatal(e)
				}
				t.Cleanup(func() { _ = os.RemoveAll(dir + ".moved") })
				if change == "replaced_root" {
					if e := os.Mkdir(dir, 0700); e != nil {
						t.Fatal(e)
					}
				}
			case "db_replace", "wal_replace", "shm_replace":
				kernel := cleanup.Admission.(*ShimCleanupAdmission)
				target := kernel.custody.databasePath
				if change == "wal_replace" {
					target += "-wal"
				}
				if change == "shm_replace" {
					target += "-shm"
				}
				if e := os.Rename(target, target+".original"); e != nil {
					t.Fatal(e)
				}
				if e := os.WriteFile(target, []byte("foreign"), 0600); e != nil {
					t.Fatal(e)
				}
				// Restore ONLY this owned fixture after assertions so original SQLite
				// handles close their own namespace normally.
				t.Cleanup(func() { _ = os.Remove(target); _ = os.Rename(target+".original", target) })
			}
		}
		if calls == 2 && change == "clock_audit" && !clockCAS {
			clockCAS = true
			r.Obligations = []string{"cleanup_pending"}
			before := r.Revision
			rev, e := adapter.Record(ctx, r, r.Revision)
			if e != nil {
				t.Fatal(e)
			}
			r.Revision = rev
			t.Logf("final Clock actual SQL CAS revision %s -> %s obligations=%v", before, rev, r.Obligations)
		}
		return now
	}
	removal := shimretire.RetentionRemoval{Cleanup: cleanup, Request: c.Request, Cursors: cursorStore, Validate: func(context.Context, shimretire.SweepRequest) error { return nil }, Observe: func(context.Context, shimretire.SweepCandidate) (shimretire.SweepCandidate, shimretire.Snapshot, shimretire.Observation, error) {
		if change == "late_hold" {
			candidate.Hold = shimretire.RetentionHeld
		}
		if change == "reminted_proof" {
			p := r.Proof
			p.ValidUntil = p.ValidUntil.Add(time.Minute)
			return candidate, r.Snapshot, p, nil
		}
		if change == "unknown_proof" {
			p := r.Proof
			p.Descendants = shimretire.ExecutionUnknown
			return candidate, r.Snapshot, p, nil
		}
		return candidate, r.Snapshot, r.Proof, nil
	}}
	if change == "no_intent" || change == "late_intent" {
		base := removal.Validate
		removal.Validate = func(ctx context.Context, req shimretire.SweepRequest) error {
			if change == "late_intent" {
				next := c.Clone()
				next.Phase = shimretire.CursorRemoved
				if _, e := cursorStore.RecordCursor(ctx, next, c.Revision); e != nil {
					return e
				}
				return nil
			}
			return base(ctx, req)
		}
		if change == "no_intent" {
			removal.Cursors = retirementCursorReadPort{CursorStore: cursorStore, load: func(ctx context.Context, id string) (shimretire.SweepCursor, error) {
				v, e := cursorStore.LoadCursor(ctx, id)
				v.Phase = shimretire.CursorReady
				v.Pending = nil
				return v, e
			}}
		}
	}
	beforeAudit, err := adapter.Load(ctx, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	beforePayload, err := os.ReadFile(name)
	if change != "missing" && (err != nil || string(beforePayload) != "private") {
		t.Fatalf("initial payload: %q %v", beforePayload, err)
	}
	started := time.Now()
	var mutation shimretire.Mutation
	if category == shimretire.Descriptor {
		cleanup.Observe = func(context.Context) (shimretire.Snapshot, shimretire.Observation, error) {
			if change == "reminted_proof" {
				p := r.Proof
				p.ValidUntil = p.ValidUntil.Add(time.Minute)
				return r.Snapshot, p, nil
			}
			if change == "unknown_proof" {
				p := r.Proof
				p.Descendants = shimretire.ExecutionUnknown
				return r.Snapshot, p, nil
			}
			return r.Snapshot, r.Proof, nil
		}
		mutation, err = cleanup.Descriptor(ctx, r.OperationID, a)
	} else {
		mutation, err = removal.Remove(ctx, candidate)
	}
	if change == "native_wait_expiry" || change == "native_guard_expiry" {
		if time.Now().Before(r.Proof.ValidUntil) || time.Since(started) > time.Second {
			t.Fatal("native wait did not refuse at original bounded expiry")
		}
	}
	if change == "sql_busy" && time.Since(started) > time.Second {
		t.Fatal("BEGIN busy wait exceeded bounded single-attempt policy")
	}
	saved, e := adapter.Load(context.Background(), r.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	_, statErr := os.Stat(name)
	if change == "renamed_root" || change == "replaced_root" {
		name = filepath.Join(dir+".moved", "host.log")
		_, statErr = os.Stat(name)
	}
	t.Logf("callbacks=%d mutation=%s error=%v obligations=%v file_exists=%t", calls, mutation, err, saved.Obligations, statErr == nil)
	if calls != 2 && change != "prior_pending" && change != "unknown_proof" && change != "no_intent" && change != "late_intent" && change != "unretired" && change != "age_floor" && change != "late_hold" {
		t.Fatal("final authority seam not reached")
	}
	if change == "missing" {
		if mutation != shimretire.NoChange || err != nil || !os.IsNotExist(statErr) {
			t.Fatalf("missing owned retry: %s %v %v", mutation, err, statErr)
		}
	} else if change == "unchanged" || (change == "prior_pending" && category == shimretire.Descriptor) {
		if mutation != shimretire.Changed || err != nil || !os.IsNotExist(statErr) || saved.Revision != beforeAudit.Revision {
			t.Fatalf("unchanged audit removal: %s %v file=%v", mutation, err, statErr)
		}
	} else {
		payload, readErr := os.ReadFile(name)
		want := "private"
		if change == "clock_replace" {
			want = "changed"
		}
		if mutation != shimretire.NoChange || err == nil || statErr != nil || readErr != nil || string(payload) != want {
			t.Fatalf("changed audit failed to retain payload: %s %v file=%v bytes=%q audit=%s", mutation, err, statErr, payload, saved.Revision)
		}
		if (change == "pending" || change == "revision" || change == "clock_audit") && saved.Revision == beforeAudit.Revision {
			t.Fatal("final callback did not commit a changed audit")
		}
		if (change == "read_error" || change == "cancel" || change == "validate_expiry" || change == "read_expiry") && saved.Revision != beforeAudit.Revision {
			t.Fatal("read refusal changed audit")
		}
		if (change == "pending" || change == "clock_audit") && (len(saved.Obligations) != 1 || saved.Obligations[0] != "cleanup_pending") {
			t.Fatalf("durable obligation lost: %v", saved.Obligations)
		}
	}
	cursor, cursorErr := cursorStore.LoadCursor(context.Background(), c.ID)
	if change == "clock_cursor" {
		if cursorErr != nil || cursor.Phase != shimretire.CursorIntent || cursor.Revision == c.Revision || cursor.Pending == nil || *cursor.Pending != *c.Pending {
			t.Fatal("actual finalClock cursor CAS not preserved")
		}
		return
	}
	if change == "late_intent" {
		if cursorErr != nil || cursor.Phase != shimretire.CursorRemoved || cursor.Revision == c.Revision {
			t.Fatal("late cursor mutation did not persist")
		}
		return
	}
	if cursorErr != nil || cursor.Revision != c.Revision || cursor.Phase != c.Phase || (c.Pending == nil && cursor.Pending != nil) || (c.Pending != nil && (cursor.Pending == nil || cursor.Pending.Artifact != a)) {
		t.Fatalf("cleanup changed durable cursor: %+v %v", cursor, cursorErr)
	}
}

type retirementCursorReadPort struct {
	shimretire.CursorStore
	load func(context.Context, string) (shimretire.SweepCursor, error)
}

func (p retirementCursorReadPort) LoadCursor(ctx context.Context, id string) (shimretire.SweepCursor, error) {
	return p.load(ctx, id)
}

// The fault stays at the final durable read, after the real authority callbacks.
type retirementAuditReadPort struct {
	shimretire.Store
	load func(context.Context, string) (shimretire.Receipt, error)
}

func (p retirementAuditReadPort) Load(ctx context.Context, operation string) (shimretire.Receipt, error) {
	return p.load(ctx, operation)
}
