//go:build linux

package store

import (
	"context"
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
		for _, change := range []string{"pending", "revision", "unchanged", "read_error", "cancel"} {
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
	for _, phase := range []shimretire.Phase{shimretire.IntentRecorded, shimretire.RetirementCommitted, shimretire.DescriptorCleanupComplete, shimretire.StateReconciled} {
		r.Phase = phase
		if phase != shimretire.IntentRecorded {
			r.Snapshot.Retired = true
			r.RetiredAt = r.Proof.ObservedAt.Add(-time.Hour)
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
	cleanup := shimretire.ConfinedCleanup{Root: root, RootID: a.RootID, OwnerID: a.OwnerID, CustodyRevision: a.CustodyRevision, RootIdentity: shimretire.FileIdentity{Device: rs.Dev, Inode: rs.Ino, Kind: "directory"}, Store: adapter, Now: func() time.Time { return r.Proof.ObservedAt }, Validate: func(context.Context, shimretire.Request) error {
		calls++
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
		if calls == 2 && change == "read_error" {
			return shimretire.Receipt{}, errors.New("final audit unavailable")
		}
		if calls == 2 && change == "cancel" {
			cancel()
		}
		return current, err
	}}
	removal := shimretire.RetentionRemoval{Cleanup: cleanup, Request: c.Request, Cursors: cursorStore, Validate: func(context.Context, shimretire.SweepRequest) error { return nil }, Observe: func(context.Context, shimretire.SweepCandidate) (shimretire.SweepCandidate, shimretire.Snapshot, shimretire.Observation, error) {
		return candidate, r.Snapshot, r.Proof, nil
	}}
	beforeAudit, err := adapter.Load(ctx, r.OperationID)
	if err != nil {
		t.Fatal(err)
	}
	beforePayload, err := os.ReadFile(name)
	if err != nil || string(beforePayload) != "private" {
		t.Fatalf("initial payload: %q %v", beforePayload, err)
	}
	var mutation shimretire.Mutation
	if category == shimretire.Descriptor {
		cleanup.Observe = func(context.Context) (shimretire.Snapshot, shimretire.Observation, error) {
			return r.Snapshot, r.Proof, nil
		}
		mutation, err = cleanup.Descriptor(ctx, r.OperationID, a)
	} else {
		mutation, err = removal.Remove(ctx, candidate)
	}
	saved, e := adapter.Load(context.Background(), r.OperationID)
	if e != nil {
		t.Fatal(e)
	}
	_, statErr := os.Stat(name)
	t.Logf("callbacks=%d mutation=%s error=%v obligations=%v file_exists=%t", calls, mutation, err, saved.Obligations, statErr == nil)
	if calls != 2 {
		t.Fatal("final authority seam not reached")
	}
	if change == "unchanged" {
		if mutation != shimretire.Changed || err != nil || !os.IsNotExist(statErr) || saved.Revision != beforeAudit.Revision {
			t.Fatalf("unchanged audit removal: %s %v file=%v", mutation, err, statErr)
		}
	} else {
		payload, readErr := os.ReadFile(name)
		if mutation != shimretire.NoChange || err == nil || statErr != nil || readErr != nil || string(payload) != string(beforePayload) {
			t.Fatalf("changed audit failed to retain payload: %s %v file=%v bytes=%q audit=%s", mutation, err, statErr, payload, saved.Revision)
		}
		if (change == "pending" || change == "revision") && saved.Revision == beforeAudit.Revision {
			t.Fatal("final callback did not commit a changed audit")
		}
		if (change == "read_error" || change == "cancel") && saved.Revision != beforeAudit.Revision {
			t.Fatal("read refusal changed audit")
		}
		if change == "pending" && (len(saved.Obligations) != 1 || saved.Obligations[0] != "cleanup_pending") {
			t.Fatalf("durable obligation lost: %v", saved.Obligations)
		}
	}
	cursor, cursorErr := cursorStore.LoadCursor(context.Background(), c.ID)
	if cursorErr != nil || cursor.Revision != c.Revision || cursor.Phase != c.Phase || (c.Pending == nil && cursor.Pending != nil) || (c.Pending != nil && (cursor.Pending == nil || *cursor.Pending != candidate)) {
		t.Fatalf("cleanup changed durable cursor: %+v %v", cursor, cursorErr)
	}
}

// The fault stays at the final durable read, after the real authority callbacks.
type retirementAuditReadPort struct {
	shimretire.Store
	load func(context.Context, string) (shimretire.Receipt, error)
}

func (p retirementAuditReadPort) Load(ctx context.Context, operation string) (shimretire.Receipt, error) {
	return p.load(ctx, operation)
}
