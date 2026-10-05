package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimretire"
)

func retirementDB(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "retirement.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Direct SQL exercises the schema boundary even when a caller bypasses the
// typed adapter. All data belongs to this test's temporary database.
func TestRetirementAuditPreventsSessionDeletion(t *testing.T) {
	for _, fk := range []string{"OFF", "ON"} {
		t.Run(fk, func(t *testing.T) {
			s := retirementDB(t)
			if _, err := s.db.Exec("PRAGMA foreign_keys=" + fk); err != nil {
				t.Fatal(err)
			}
			var enabled int
			if err := s.db.QueryRow("PRAGMA foreign_keys").Scan(&enabled); err != nil {
				t.Fatal(err)
			}
			if (enabled == 1) != (fk == "ON") {
				t.Fatal("foreign key configuration mismatch")
			}
			for _, phase := range []shimretire.Phase{shimretire.IntentRecorded, shimretire.RetirementCommitted, shimretire.DescriptorCleanupComplete, shimretire.StateReconciled} {
				id := string(phase)
				if _, err := s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
					t.Fatal(err)
				}
				state := "orphaned"
				if phase == shimretire.IntentRecorded {
					state = "running"
				}
				if phase == shimretire.RetirementCommitted {
					state = "detached"
				}
				if err := s.CreateSession(SessionRow{ID: id, State: state}, &launch.Plan{}); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`INSERT INTO session_shim_retirements(operation_id,session_id,request_digest,revision,phase,record_json,created_at,updated_at) VALUES(?,?,?,?,?,'{}','now','now')`, id, id, "digest", "revision", id); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec("PRAGMA foreign_keys=" + fk); err != nil {
					t.Fatal(err)
				}
				if _, err := s.db.Exec(`DELETE FROM sessions WHERE id=?`, id); err == nil {
					t.Fatalf("%s: audit-bearing session deleted", phase)
				}
				for _, table := range []string{"sessions", "session_shim_retirements"} {
					var n int
					column := "session_id"
					if table == "sessions" {
						column = "id"
					}
					if err := s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE "+column+"=?", id).Scan(&n); err != nil || n != 1 {
						t.Fatalf("%s %s retained rows=%d err=%v", phase, table, n, err)
					}
				}
			}
			if _, err := s.db.Exec("PRAGMA foreign_keys=OFF"); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateSession(SessionRow{ID: "unaudited", State: "created"}, &launch.Plan{}); err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec("PRAGMA foreign_keys=" + fk); err != nil {
				t.Fatal(err)
			}
			result, err := s.db.Exec(`DELETE FROM sessions WHERE id='unaudited'`)
			if err != nil {
				t.Fatal(err)
			}
			n, err := result.RowsAffected()
			if err != nil || n != 1 {
				t.Fatalf("unaudited control: %d %v", n, err)
			}
		})
	}
}

func retirementReceipt() shimretire.Receipt {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p := shimretire.Placement{Session: "session", Instance: "instance", OperationKey: "launch", Revision: "placement", Generation: 1}
	u := shimretire.Unit{Manager: "manager", Name: "unit", Invocation: "invocation", Attempt: "attempt", ScopeRevision: "scope"}
	j := shimretire.JournalIdentity{RootID: "journal-root", ID: "journal", Session: p.Session, HighWater: "journal:1", Generation: 1}
	r := shimretire.Request{Version: shimretire.Version, OperationID: "retire", ActorID: "operator", AuthorizationID: "grant", AuthorizationRevision: "grant-revision", Reason: "operator_request", Placement: p, PolicyID: "keep", PolicyRevision: "policy"}
	raw, _ := json.Marshal(r)
	digest := sha256.Sum256(raw)
	snapshot := shimretire.Snapshot{Placement: p, Backend: "systemd-user", Unit: u, Journal: j, InventoryRevision: "inventory", Descriptor: shimretire.Artifact{RootID: "root", RelativePath: "launch.json", OwnerID: "owner", CustodyRevision: "custody", InventoryRevision: "inventory", Category: shimretire.Descriptor, Identity: shimretire.FileIdentity{Device: 1, Inode: 2, Kind: "regular"}}}
	proof := shimretire.Observation{Placement: p, Unit: u, Journal: j, Issuer: "host", HostBoot: "boot", Revision: "observation", Fence: "fence", Scope: "scope", ObservedAt: now, ValidUntil: now.Add(time.Second), Submission: shimretire.DrainedFenced, Containment: shimretire.OwnedAllDescendants, Host: shimretire.Absent, Descendants: shimretire.Absent, Controller: shimretire.Absent, JournalWriter: shimretire.Absent}
	return shimretire.Receipt{Version: shimretire.Version, OperationID: r.OperationID, RequestDigest: hex.EncodeToString(digest[:]), Request: r, Snapshot: snapshot, Proof: proof, Phase: shimretire.IntentRecorded}
}

func TestRetirementStoreBindingAndCAS(t *testing.T) {
	s := retirementDB(t)
	ctx := context.Background()
	adapter := ShimRetirementStore{Store: s}
	r := retirementReceipt()
	if _, err := adapter.Record(ctx, r, ""); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("missing session: %v", err)
	}
	if err := s.CreateSession(SessionRow{ID: r.Request.Placement.Session, State: "created"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	rev, err := adapter.Record(ctx, r, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Revision = rev
	got, err := adapter.Load(ctx, r.OperationID)
	if err != nil || got.Request != r.Request || got.Revision != rev {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, change := range []func(*shimretire.Receipt){
		func(v *shimretire.Receipt) { v.Revision = "stale" },
		func(v *shimretire.Receipt) { v.Snapshot.Descriptor.Identity.Inode++ },
		func(v *shimretire.Receipt) {
			v.Phase = shimretire.StateReconciled
			v.Snapshot.Retired = true
			v.RetiredAt = v.Proof.ObservedAt
		},
		func(v *shimretire.Receipt) { v.RequestDigest = "foreign" },
	} {
		bad := r.Clone()
		change(&bad)
		if _, err := adapter.Record(ctx, bad, bad.Revision); !errors.Is(err, ErrShimRetirementConflict) {
			t.Fatalf("bad record accepted: %v", err)
		}
	}
	r.Obligations = []string{"cleanup_pending"}
	rev, err = adapter.Record(ctx, r, r.Revision)
	if err != nil {
		t.Fatal(err)
	}
	r.Revision = rev
	bad := r.Clone()
	bad.Obligations = nil
	if _, err := adapter.Record(ctx, bad, rev); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("obligation discarded: %v", err)
	}
	r.Phase = shimretire.RetirementCommitted
	r.Snapshot.Retired = true
	r.RetiredAt = r.Proof.ObservedAt
	if _, err := adapter.Record(ctx, r, rev); err != nil {
		t.Fatal(err)
	}
}

func TestRetirementReconciliationRequiresCleanupAndPreservesExit(t *testing.T) {
	s := retirementDB(t)
	ctx := context.Background()
	adapter := ShimRetirementStore{Store: s}
	r := retirementReceipt()
	if err := s.CreateSession(SessionRow{ID: r.Request.Placement.Session, State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSessionShim(ctx, SessionShimRow{SessionID: r.Request.Placement.Session, ShimKey: r.Request.Placement.OperationKey, HostBackend: r.Snapshot.Backend, UnitName: r.Snapshot.Unit.Name, SocketPath: "/private/socket", DescriptorPath: "/private/launch.json", JournalID: r.Snapshot.Journal.ID, Runtime: "fixture", RuntimeGeneration: r.Request.Placement.Generation, BootGeneration: "boot"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO principals(principal_id,kind,display,token_hash,scopes_json,session_id,addresses_json,created_at) VALUES('principal','session','fixture',?,'[]',?,'[]','now')`, strings.Repeat("0", 64), r.Request.Placement.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO runtime_bindings(id,target_urn,session_id,host_id,attempt_id,generation,leased_at,created_at,updated_at) VALUES('binding','msg://session/local/session',?,'host','attempt',1,'now','now','now')`, r.Request.Placement.Session); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE sessions SET pid=123,pid_started_at='fixture' WHERE id=?`, r.Request.Placement.Session); err != nil {
		t.Fatal(err)
	}
	rev, err := adapter.Record(ctx, r, "")
	if err != nil {
		t.Fatal(err)
	}
	r.Revision = rev
	if _, err = adapter.ReconcileRetirement(ctx, r.Request.Placement, r.OperationID); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("intent reconciled: %v", err)
	}
	r.Phase = shimretire.RetirementCommitted
	r.Snapshot.Retired = true
	r.RetiredAt = r.Proof.ObservedAt
	rev, err = adapter.Record(ctx, r, rev)
	if err != nil {
		t.Fatal(err)
	}
	r.Revision = rev
	if _, err = adapter.ReconcileRetirement(ctx, r.Request.Placement, r.OperationID); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("pending cleanup reconciled: %v", err)
	}
	for _, table := range []string{"principals", "runtime_bindings"} {
		var active int
		if err = s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE session_id=? AND revoked_at IS NULL", r.Request.Placement.Session).Scan(&active); err != nil || active != 1 {
			t.Fatalf("premature revocation %s: %d %v", table, active, err)
		}
	}
	r.Phase = shimretire.DescriptorCleanupComplete
	rev, err = adapter.Record(ctx, r, rev)
	if err != nil {
		t.Fatal(err)
	}
	r.Revision = rev
	change, err := adapter.ReconcileRetirement(ctx, r.Request.Placement, r.OperationID)
	if err != nil || change.From != "running" || change.To != "orphaned" {
		t.Fatalf("reconcile: %+v %v", change, err)
	}
	var state string
	var exit, ended any
	if err = s.db.QueryRow(`SELECT state,exit_code,ended_at FROM sessions WHERE id=?`, r.Request.Placement.Session).Scan(&state, &exit, &ended); err != nil || state != "orphaned" || exit != nil || ended != nil {
		t.Fatalf("invented exit: %s %v %v %v", state, exit, ended, err)
	}
	for _, table := range []string{"principals", "runtime_bindings"} {
		var active int
		if err = s.db.QueryRow("SELECT count(*) FROM "+table+" WHERE session_id=? AND revoked_at IS NULL", r.Request.Placement.Session).Scan(&active); err != nil || active != 0 {
			t.Fatalf("authority retained %s: %d %v", table, active, err)
		}
	}
	var pid any
	if err = s.db.QueryRow(`SELECT pid FROM sessions WHERE id=?`, r.Request.Placement.Session).Scan(&pid); err != nil || pid != nil {
		t.Fatalf("retired PID retained: %v %v", pid, err)
	}
	bad := r.Request.Placement
	bad.Generation++
	if _, err = adapter.ReconcileRetirement(ctx, bad, r.OperationID); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("replacement generation reconciled: %v", err)
	}
}

func retentionCursorFixture() shimretire.SweepCursor {
	age := time.Hour
	r := shimretire.SweepRequest{Version: shimretire.RetentionVersion, OperationID: "cursor", ActorID: "operator", AuthorizationID: "grant", AuthorizationRevision: "grant-revision", ScopeRevision: "scope", Policy: shimretire.RetentionPolicy{Version: shimretire.RetentionVersion, ID: "policy", Revision: "revision", Mode: shimretire.ExplicitBounded, Categories: []shimretire.Category{shimretire.HostLog}, RootIDs: []string{"root"}, MinRetiredAge: &age}, Budget: shimretire.SweepBudget{MaxExamined: 10, MaxMutated: 2, MaxBytesExamined: 100, MaxBytesRemoved: 100, MaxDuration: time.Second}}
	raw, _ := json.Marshal(r)
	digest := sha256.Sum256(raw)
	return shimretire.SweepCursor{Version: shimretire.RetentionVersion, ID: r.OperationID, Request: r, RequestDigest: hex.EncodeToString(digest[:]), InventoryRevision: "inventory", Phase: shimretire.CursorReady}
}
func TestRetentionCursorCASAndPendingPreservation(t *testing.T) {
	s := retirementDB(t)
	ctx := context.Background()
	adapter := ShimRetentionStore{Store: s}
	c := retentionCursorFixture()
	rev, err := adapter.RecordCursor(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = rev
	got, err := adapter.LoadCursor(ctx, c.ID)
	if err != nil || got.RequestDigest != c.RequestDigest || got.Revision != rev {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	candidate := shimretire.SweepCandidate{RetirementOperation: "retire", Artifact: shimretire.Artifact{RootID: "root", RelativePath: "host.log", OwnerID: "owner", CustodyRevision: "custody", InventoryRevision: "inventory", Identity: shimretire.FileIdentity{Device: 1, Inode: 2, Kind: "regular"}, Category: shimretire.HostLog, Size: 7}, SortKey: "retired/session/host.log", Hold: shimretire.NoRetentionHold, HoldRevision: "holds"}
	c.Pending = &candidate
	c.Phase = shimretire.CursorIntent
	rev, err = adapter.RecordCursor(ctx, c, rev)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = rev
	bad := c.Clone()
	bad.Pending = nil
	bad.Phase = shimretire.CursorReady
	bad.LastKey = candidate.SortKey
	if _, err = adapter.RecordCursor(ctx, bad, rev); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("pending skipped: %v", err)
	}
	bad = c.Clone()
	bad.Pending.Artifact.Identity.Inode++
	if _, err = adapter.RecordCursor(ctx, bad, rev); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("replacement pending: %v", err)
	}
	c.Phase = shimretire.CursorRemoved
	next, err := adapter.RecordCursor(ctx, c, rev)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = adapter.RecordCursor(ctx, c, rev); !errors.Is(err, ErrShimRetirementConflict) {
		t.Fatalf("stale CAS: %v", err)
	}
	c.Revision = next
	c.Pending = nil
	c.Phase = shimretire.CursorReady
	c.LastKey = candidate.SortKey
	next, err = adapter.RecordCursor(ctx, c, next)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = next
	c.Retained = []shimretire.RetainedItem{{Candidate: candidate, Code: "retention_hold"}}
	next, err = adapter.RecordCursor(ctx, c, next)
	if err != nil {
		t.Fatal(err)
	}
	c.Revision = next
	for _, replacement := range [][]shimretire.RetainedItem{nil, {{Candidate: candidate, Code: "inspect_only"}}} {
		bad = c.Clone()
		bad.Retained = replacement
		if _, err = adapter.RecordCursor(ctx, bad, next); !errors.Is(err, ErrShimRetirementConflict) {
			t.Fatalf("retained recovery replaced: %v", err)
		}
	}
}

func TestRetirementRefusesWeakDurability(t *testing.T) {
	s := retirementDB(t)
	ctx := context.Background()
	r := retirementReceipt()
	if err := s.CreateSession(SessionRow{ID: r.Request.Placement.Session, State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"OFF", "NORMAL"} {
		if _, err := s.db.Exec("PRAGMA synchronous=" + mode); err != nil {
			t.Fatal(err)
		}
		if _, err := (ShimRetirementStore{Store: s}).Record(ctx, r, ""); err == nil {
			t.Fatalf("%s accepted as durable retirement", mode)
		}
		if _, err := (ShimRetentionStore{Store: s}).RecordCursor(ctx, retentionCursorFixture(), ""); err == nil {
			t.Fatalf("%s accepted as durable cursor", mode)
		}
	}
	if _, err := s.db.Exec("PRAGMA synchronous=FULL"); err != nil {
		t.Fatal(err)
	}
	if _, err := (ShimRetirementStore{Store: s}).Record(ctx, r, ""); err != nil {
		t.Fatalf("FULL refusal: %v", err)
	}
}
