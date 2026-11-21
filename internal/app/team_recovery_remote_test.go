package app

import (
	"context"
	"testing"

	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func remoteRecoveryExit(t *testing.T, svc *Service, id, worker string, scoped bool) {
	t.Helper()
	recordedExitFixture(t, svc, id, worker, 1)
	if _, err := svc.Store.DB().Exec(`UPDATE sessions SET state='completed',exit_code=NULL WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	payload := `{"from":"killed","to":"completed","exit_code":null,"reason":"worker_exit"}`
	seq, _, err := svc.Store.InsertEvent(events.ScopeSession, id, events.KindSessionStateChanged, payload)
	if err != nil {
		t.Fatal(err)
	}
	if scoped {
		if _, err = svc.Store.DB().Exec(`UPDATE events SET scope_worker_id=? WHERE id=?`, worker, seq); err != nil {
			t.Fatal(err)
		}
	}
}

func TestTeamSessionRecoveryRemoteExitEvidence(t *testing.T) {
	for _, tc := range []string{"confirmed-null-exit", "missing-worker-evidence", "wrong-worker", "later-custody-disconnected", "launch-pending"} {
		t.Run(tc, func(t *testing.T) {
			svc := stopHarness(t)
			remoteRecoveryExit(t, svc, "remote", "worker", tc != "missing-worker-evidence")
			switch tc {
			case "wrong-worker":
				if _, err := svc.Store.DB().Exec(`UPDATE events SET scope_worker_id='foreign' WHERE session_id='remote'`); err != nil {
					t.Fatal(err)
				}
			case "later-custody-disconnected":
				if _, _, err := svc.Store.InsertEvent(events.ScopeSession, "remote", KindSessionKillOrdered, `{"worker_id":"worker"}`); err != nil {
					t.Fatal(err)
				}
			case "launch-pending":
				unlock, err := svc.lockSessionLaunch(context.Background(), "remote")
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			ready, err := svc.TeamSessionRecoveryReady(context.Background(), "remote")
			if err != nil || ready != (tc == "confirmed-null-exit") {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}

func TestTeamSessionRecoveryRemoteInventoryContradictions(t *testing.T) {
	for _, tc := range []string{"complete-absent", "incomplete-inventory", "live-inventory"} {
		t.Run(tc, func(t *testing.T) {
			var restore *workerwire.RestoreCounts
			if tc != "incomplete-inventory" {
				restore = &workerwire.RestoreCounts{}
			}
			r := newIdentityRigWithRestore(t, restore, workerwire.FeatureSessionCred)
			if tc == "live-inventory" {
				if _, err := r.launch("remote", r.urn); err != nil {
					t.Fatal(err)
				}
				if _, err := r.db.DB().Exec(`UPDATE sessions SET state='completed',pid=0,exit_code=NULL,ended_at='2026-11-20T19:45:36Z' WHERE id='remote'`); err != nil {
					t.Fatal(err)
				}
				if _, _, err := r.db.InsertEvent(events.ScopeSession, "remote", events.KindSessionStateChanged, `{"to":"completed","reason":"worker_exit","worker_id":"`+r.worker+`"}`); err != nil {
					t.Fatal(err)
				}
			} else {
				remoteRecoveryExit(t, r.svc, "remote", r.worker, true)
			}
			if _, _, err := r.db.InsertEvent(events.ScopeSession, "remote", KindSessionKillOrdered, `{"worker_id":"`+r.worker+`"}`); err != nil {
				t.Fatal(err)
			}
			ready, err := r.svc.TeamSessionRecoveryReady(context.Background(), "remote")
			if err != nil || ready != (tc == "complete-absent") {
				t.Fatalf("ready=%v err=%v", ready, err)
			}
		})
	}
}
