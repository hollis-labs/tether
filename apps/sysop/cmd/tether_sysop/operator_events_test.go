package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/environmentstream"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

const operatorTestEnv = "f1e26f77-f1a7-4ca2-a991-e834a9090808"

func TestOperatorObservationAllowlist(t *testing.T) {
	for _, kind := range []string{kindSysopSessionStop, kindSysopSessionCheckpoint, kindSysopLogicalResume, kindSysopSessionCleanup, kindSysopLogicalAgentPolicy} {
		out, err := operatorObservation(kind, map[string]any{"summary": "secret body", "status": "secret body", "checkpoint_status": "secret body", "body": "secret body", "actor": "secret body", "source": "forged", "action": "forged", "summary_bytes": 11, "session_ids": []string{"secret body"}, "logical_agent_id": "known-agent", "checkpoint_policy": "on_stop", "provider_id": strings.Repeat("x", 257), "deleted_count": 2})
		raw, _ := json.Marshal(out)
		if err != nil || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "forged") || strings.Contains(string(raw), strings.Repeat("x", 257)) {
			t.Fatal(kind, string(raw), err)
		}
	}
	if _, err := operatorObservation("unknown", nil); err == nil {
		t.Fatal("unknown kind admitted")
	}
}

func TestOperatorCheckpointKeepsBodyOnlyInCheckpoint(t *testing.T) {
	for _, failAudit := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded", true: "audit-unavailable"}[failAudit], func(t *testing.T) {
			s, db := inboxFixture(t)
			if err := db.CreateSession(store.SessionRow{ID: "s1", State: "running", LogicalAgentID: "agent", ProjectID: "project", WorkerID: "worker", LaunchID: "launch"}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := db.SeedEventIdentity(context.Background(), operatorTestEnv); err != nil {
				t.Fatal(err)
			}
			if failAudit {
				if _, err := db.DB().Exec(`CREATE TRIGGER reject_operator BEFORE INSERT ON events WHEN NEW.kind='sysop.session_checkpoint' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
					t.Fatal(err)
				}
			}
			secret := "secret-bearing checkpoint summary"
			status := "private checkpoint status"
			daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					Status  string `json:"status"`
					Summary string `json:"summary"`
				}
				if r.Method != http.MethodPost || r.URL.Path != "/sessions/s1/checkpoint" || json.NewDecoder(r.Body).Decode(&req) != nil || req.Summary != secret || req.Status != status {
					t.Error("authoritative action changed")
					w.WriteHeader(400)
					return
				}
				if err := db.CreateCheckpoint(checkpoint.Checkpoint{ID: "checkpoint", LogicalAgentID: "agent", SourceSessionID: "s1", Summary: req.Summary, Status: req.Status, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}); err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				w.WriteHeader(http.StatusCreated)
			}))
			defer daemon.Close()
			dbPath := filepath.Join(s.catalogRoot, "state.db")
			cfg := fmt.Sprintf("catalog:\n  defaults:\n    state_db: %q\ndaemon:\n  listen_addr: %q\n", dbPath, "tcp:"+strings.TrimPrefix(daemon.URL, "http://"))
			if err := os.WriteFile(filepath.Join(s.catalogRoot, "global.yaml"), []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			body, _ := json.Marshal(checkpointActionRequest{ID: "s1", Status: status, Summary: secret})
			rec := httptest.NewRecorder()
			s.handleSessionCheckpoint(rec, httptest.NewRequest(http.MethodPost, "/api/sessions/checkpoint", strings.NewReader(string(body))))
			if rec.Code != 200 {
				t.Fatalf("action %d %s", rec.Code, rec.Body.String())
			}
			cp, err := db.GetCheckpoint("checkpoint")
			if err != nil || cp.Summary != secret || cp.Status != status {
				t.Fatal("checkpoint content changed", cp, err)
			}
			pending, err := db.PendingEventsContext(context.Background(), 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			if failAudit {
				if len(pending) != 0 {
					t.Fatal("failed audit left partial outbox", pending)
				}
				return
			}
			if len(pending) != 1 {
				t.Fatalf("outbox %+v", pending)
			}
			e := pending[0]
			if !strings.HasPrefix(e.EventID, "evt_") || e.Envelope.App != "tether-sysop" || e.Envelope.Dims.EnvironmentID != operatorTestEnv || e.Envelope.Dims.ProjectID != "project" || e.LogicalAgentID != "agent" || e.Envelope.Dims.WorkerID != "worker" || !e.Envelope.Class.Sanitized || e.Envelope.Actor.URN != "" {
				t.Fatalf("identity/stamp %+v", e)
			}
			if strings.Contains(e.PayloadJSON, secret) || strings.Contains(e.PayloadJSON, status) || strings.Contains(e.PayloadJSON, "summary\"") {
				t.Fatal("checkpoint content leaked", e.PayloadJSON)
			}
			var observed map[string]any
			_ = json.Unmarshal([]byte(e.PayloadJSON), &observed)
			if observed["summary_bytes"] != float64(len(secret)) {
				t.Fatal(observed)
			}
			assertOperatorStream(t, db, e.EventID, secret, status)
		})
	}
}

func assertOperatorStream(t *testing.T, db *store.Store, id string, forbidden ...string) {
	t.Helper()
	bus := events.NewBus(events.BusOptions{Persister: db})
	projection := environmentstream.New(operatorTestEnv, db, bus)
	projection.StoredIDs = &environmentstream.StoredIDProjection{Cutover: 0}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { projection.Stream(w, r, "") }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"?after_seq=0", nil)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal("stream status", resp.StatusCode)
	}
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		for _, secret := range forbidden {
			if strings.Contains(line, secret) {
				t.Fatal("stream leaked checkpoint content")
			}
		}
		if strings.Contains(line, id) && strings.HasPrefix(line, "data:") {
			return
		}
	}
	t.Fatal("missing operator event in stream", scanner.Err())
}

func TestOperatorOutboxDaemonOrderAndInsertRollback(t *testing.T) {
	_, db := inboxFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// The daemon bus is already running and clean; Sysop's separate bus
	// cannot set its in-memory dirty flag or kick channel.
	bus := events.NewBus(events.BusOptions{Persister: db})
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "test.bootstrap"}); err != nil {
		t.Fatal(err)
	}
	kinds := []string{kindSysopSessionStop, kindSysopSessionCheckpoint, kindSysopLogicalResume, kindSysopSessionCleanup, kindSysopLogicalAgentPolicy}
	for _, kind := range kinds {
		if err := enqueueOperatorEvent(ctx, db, events.ScopeSession, "s1", kind, nil); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := db.PendingEventsContext(ctx, 0, 10)
	if err != nil || len(pending) != len(kinds) {
		t.Fatal(pending, err)
	}
	var ids []string
	seen := map[string]bool{}
	for _, e := range pending {
		if !strings.HasPrefix(e.EventID, "evt_") || seen[e.EventID] || e.Envelope.App != "tether-sysop" || !e.Envelope.Class.Sanitized || e.Envelope.Actor.URN != "" {
			t.Fatal("operator envelope", e)
		}
		ids = append(ids, e.EventID)
		seen[e.EventID] = true
	}
	live, stop, err := bus.(events.LiveSubscriber).SubscribeLive(ctx, events.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	if err := bus.Publish(ctx, events.Event{Scope: events.ScopeDaemon, Kind: "test.after"}); err != nil {
		t.Fatal(err)
	}
	for i, want := range append(ids, "") {
		select {
		case got := <-live:
			if (i < len(kinds) && got.EventID != want) || (i == len(kinds) && got.Kind != "test.after") {
				t.Fatal("outbox reordered/reminted", got)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	pending, err = db.PendingEventsContext(ctx, 0, 10)
	if err != nil || len(pending) != 0 {
		t.Fatal("daemon did not settle outbox", pending, err)
	}
	if _, err := db.DB().Exec(`CREATE TRIGGER reject_operator BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	if err := enqueueOperatorEvent(ctx, db, events.ScopeSession, "s1", kindSysopSessionStop, nil); err == nil {
		t.Fatal("failed insert reported success")
	}
	es, err := db.EventsSince(0)
	if err != nil || len(es) != len(kinds)+2 {
		t.Fatal("failed insert changed log", es, err)
	}
}
