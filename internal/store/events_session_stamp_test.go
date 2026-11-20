package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
)

func sessionStampFixture(t *testing.T) *Store {
	t.Helper()
	s := openTestStoreForSessions(t)
	if err := s.CreateSession(SessionRow{ID: "subject", LaunchID: "l", ProjectID: "p", LogicalAgentID: "a", ProviderID: "provider", Workspace: "/tmp/ws", State: "created", TaskID: sql.NullString{String: "task", Valid: true}, WorkstreamID: sql.NullString{String: "stream", Valid: true}}, &launch.Plan{LaunchID: "l"}); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO session_groups(id,created_at,updated_at) VALUES('group','now','now')`,
		`INSERT INTO team_definitions(team_id,version,payload) VALUES('team',1,'{}')`,
		`INSERT INTO team_runs(run_id,session_group_id,team_id,team_version,payload,created_at) VALUES('run','group','team',1,'{}','now')`,
		`UPDATE sessions SET session_group_id='group' WHERE id='subject'`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for _, m := range []struct{ owner, provider, native string }{{"tether", "provider", "native"}, {"foreign", "provider", "wrong-owner"}, {"tether", "other", "other-native"}} {
		if err := s.UpsertSessionProviderMapping("subject", m.owner, m.provider, m.native); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestStoredSessionStampAllWritePaths(t *testing.T) {
	for _, path := range []string{"envelope", "legacy", "transaction"} {
		t.Run(path, func(t *testing.T) {
			s := sessionStampFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Caller attribution must not be mistaken for the event's subject.
			ctx = callcontext.WithSnapshot(ctx, callcontext.Snapshot{SessionResolved: true, SessionID: "caller", TaskID: "wrong"})
			e := events.Event{Scope: events.ScopeSession, SessionID: "subject", Kind: "session.output", PayloadJSON: `{"task_id":"untrusted"}`}
			var got events.Event
			var err error
			switch path {
			case "legacy":
				seq, _, writeErr := s.InsertEventContext(ctx, e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
				if writeErr != nil {
					t.Fatal(writeErr)
				}
				rows, readErr := s.EventsSince(seq - 1)
				if readErr != nil {
					t.Fatal(readErr)
				}
				got = rows[0]
			case "transaction":
				tx, txErr := s.db.BeginTx(ctx, nil)
				if txErr != nil {
					t.Fatal(txErr)
				}
				defer tx.Rollback()
				if _, err = tx.Exec(`UPDATE sessions SET task_id='uncommitted' WHERE id='subject'`); err != nil {
					t.Fatal(err)
				}
				got, err = s.InsertEventTx(ctx, tx, e)
				if err == nil {
					err = tx.Commit()
				}
			default:
				got, err = s.InsertEventEnvelope(ctx, e)
			}
			if err != nil {
				t.Fatal(err)
			}
			task := "task"
			if path == "transaction" {
				task = "uncommitted"
			}
			if got.Envelope.Dims.TaskID != task || got.Envelope.Dims.WorkstreamID != "stream" || got.Envelope.Dims.RunID != "run" || got.Envelope.Process.ProviderSessionID != "native" {
				t.Fatalf("missing/wrong stamp: %+v", got.Envelope)
			}
			row := rowByID(t, s, got.Seq)
			if row["scope_task_id"] != task || row["provider_session_id"] != "native" {
				t.Fatalf("persisted stamp: %#v", row)
			}
		})
	}
}

func TestStoredSessionStampPreservesExplicitAndUnknown(t *testing.T) {
	s := sessionStampFixture(t)
	for _, session := range []string{"subject", "unknown", ""} {
		e := events.Event{Scope: events.ScopeSession, SessionID: session, Kind: "test", Envelope: events.Envelope{Dims: events.ScopeDims{TaskID: "explicit-task", WorkstreamID: "explicit-stream", RunID: "explicit-run"}, Process: events.Process{ProviderSessionID: "explicit-native"}}}
		got, err := s.InsertEventEnvelope(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if got.Envelope.Dims != e.Envelope.Dims || got.Envelope.Process.ProviderSessionID != "explicit-native" {
			t.Fatalf("overwrote explicit stamp: %+v", got.Envelope)
		}
	}
	for _, session := range []string{"unknown", ""} {
		got, err := s.InsertEventEnvelope(context.Background(), events.Event{Scope: events.ScopeDaemon, SessionID: session, Kind: "test"})
		if err != nil {
			t.Fatal(err)
		}
		if got.Envelope.Dims.TaskID != "" || got.Envelope.Process.ProviderSessionID != "" {
			t.Fatalf("invented attribution: %+v", got.Envelope)
		}
	}
}

func TestStoredSessionStampProviderIsolationAndLateMapping(t *testing.T) {
	s := sessionStampFixture(t)
	for _, tc := range []struct{ provider, want string }{{"other", "other-native"}, {"missing", ""}} {
		e := events.Event{Scope: events.ScopeSession, SessionID: "subject", Kind: "test"}
		e.Envelope.Process.Provider = tc.provider
		got, err := s.InsertEventEnvelope(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if got.Envelope.Process.ProviderSessionID != tc.want {
			t.Fatalf("provider %q: got %q want %q", tc.provider, got.Envelope.Process.ProviderSessionID, tc.want)
		}
	}
	if err := s.UpsertSessionProviderMapping("subject", "tether", "provider", ""); err != nil {
		t.Fatal(err)
	}
	e := events.Event{Scope: events.ScopeSession, SessionID: "subject", Kind: "test", EventID: "stable-id"}
	first, err := s.InsertEventEnvelope(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if first.Envelope.Process.ProviderSessionID != "" {
		t.Fatal("empty mapping should remain empty")
	}
	if err := s.UpsertSessionProviderMapping("subject", "tether", "provider", "late"); err != nil {
		t.Fatal(err)
	}
	again, err := s.InsertEventEnvelope(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if again.Seq != first.Seq || again.Envelope.Process.ProviderSessionID != "" {
		t.Fatal("retry rewrote historical event")
	}
	e.EventID = ""
	next, err := s.InsertEventEnvelope(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if next.Envelope.Process.ProviderSessionID != "late" {
		t.Fatal("late mapping was cached away")
	}
}

func TestStoredSessionStampUnresolvedContextCannotInfer(t *testing.T) {
	for _, path := range []string{"envelope", "legacy", "transaction"} {
		for _, verified := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/verified=%v", path, verified), func(t *testing.T) {
				s := sessionStampFixture(t)
				ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{
					Verified: verified, SessionResolved: false, SessionID: "subject", TaskID: "claimed-task", WorkstreamID: "claimed-stream",
				})
				e := events.Event{Scope: events.ScopeSession, SessionID: "subject", Kind: "test"}
				var got events.Event
				var err error
				switch path {
				case "legacy":
					seq, _, writeErr := s.InsertEventContext(ctx, e.Scope, e.SessionID, e.Kind, e.PayloadJSON)
					if writeErr != nil {
						t.Fatal(writeErr)
					}
					rows, readErr := s.EventsSince(seq - 1)
					if readErr != nil {
						t.Fatal(readErr)
					}
					got = rows[0]
				case "transaction":
					tx, txErr := s.db.BeginTx(ctx, nil)
					if txErr != nil {
						t.Fatal(txErr)
					}
					defer tx.Rollback()
					got, err = s.InsertEventTx(ctx, tx, e)
					if err == nil {
						err = tx.Commit()
					}
				default:
					got, err = s.InsertEventEnvelope(ctx, e)
				}
				if err != nil {
					t.Fatal(err)
				}
				if got.Envelope.Dims.TaskID != "" || got.Envelope.Dims.WorkstreamID != "" || got.Envelope.Dims.RunID != "" || got.Envelope.Process.ProviderSessionID != "" {
					t.Fatalf("unresolved caller gained attribution: %+v", got.Envelope)
				}
				row := rowByID(t, s, got.Seq)
				for _, column := range []string{"scope_task_id", "scope_workstream_id", "scope_run_id", "provider_session_id"} {
					if row[column] != nil {
						t.Fatalf("unresolved caller persisted %s=%v", column, row[column])
					}
				}
			})
		}
	}
	// Skipping inference does not erase explicitly supplied producer metadata.
	s := sessionStampFixture(t)
	ctx := callcontext.WithSnapshot(context.Background(), callcontext.Snapshot{})
	e := events.Event{Scope: events.ScopeSession, SessionID: "subject", Kind: "test", Envelope: events.Envelope{Dims: events.ScopeDims{TaskID: "explicit", WorkstreamID: "explicit", RunID: "explicit"}, Process: events.Process{ProviderSessionID: "explicit"}}}
	got, err := s.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if got.Envelope.Dims != e.Envelope.Dims || got.Envelope.Process.ProviderSessionID != "explicit" {
		t.Fatal("unresolved context erased explicit metadata")
	}
}
