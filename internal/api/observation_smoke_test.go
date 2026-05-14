// Package api — observation contract smoke tests (CW-20260423-0023)
//
// This file exercises the observation surface defined in ADR 0024 as a
// consumer would: spin up an in-process httptest server backed by minimal
// fake stores, call the observation endpoints, and assert the response shape
// matches the durability/replay contract.
//
// Covered scenarios:
//
//	S1: Session lifecycle visibility     — GET /sessions, GET /sessions/{id}, GET /sessions/{id}/events
//	S2: Terminal outcome visibility      — GET /sessions/{id} with state=completed + exit_code
//	S3: Checkpoint visibility by session — GET /sessions/{id}/checkpoints
//	S4: Client attachment history        — GET /sessions/{id}/attachments
//	S5: Proxy/tool call event visibility — GET /proxy/events with session_id, errors_only, limit filters
//	S6: Reconnect/replay simulation      — GET /sessions/{id}/events?cursor=N paging
//
// Gaps documented (not covered here):
//
//	SSE stream replay (GET /events/stream)   — covered by events_test.go broker tests
//	GET /proxy/events?since= timestamp filter — thin wrapper around store; store tested in store package
//	GET /logical-agents/{id}/checkpoints     — covered by checkpoints_test.go
package api

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

// ─── minimal fake stores for observation tests ────────────────────────────────

// obsEventsStore is a minimal EventsStore for observation tests. It supports
// optional cursor filtering to simulate the replay paging contract.
type obsEventsStore struct {
	byID map[string][]events.Event
}

func (f *obsEventsStore) ListEventsBySession(id string, limit int, cursor int64) ([]events.Event, error) {
	all := f.byID[id]
	var out []events.Event
	for _, e := range all {
		// cursor > 0 means: return only events with seq < cursor (paging backwards)
		if cursor > 0 && e.Seq >= cursor {
			continue
		}
		out = append(out, e)
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// obsCheckpointStore is a minimal CheckpointStore for observation tests.
type obsCheckpointStore struct {
	byAgent map[string][]checkpoint.Checkpoint
}

func (f *obsCheckpointStore) CreateCheckpoint(c checkpoint.Checkpoint) error { return nil }

func (f *obsCheckpointStore) GetLatestCheckpointForAgent(_ string) (*checkpoint.Checkpoint, error) {
	return nil, nil
}

func (f *obsCheckpointStore) ListLogicalAgents() ([]store.LogicalAgentRow, error) { return nil, nil }

func (f *obsCheckpointStore) ListCheckpointsByLogicalAgent(agentID string) ([]checkpoint.Checkpoint, error) {
	return f.byAgent[agentID], nil
}

// obsAttachmentStore is a minimal AttachmentStore for observation tests.
type obsAttachmentStore struct {
	bySession map[string][]store.ClientAttachmentRow
}

func (f *obsAttachmentStore) ListClientAttachments(sessionID string) ([]store.ClientAttachmentRow, error) {
	return f.bySession[sessionID], nil
}

// obsProxyEventStore is a minimal ProxyEventStore for observation tests.
// It holds events in a slice and applies the same filter logic as the real store.
type obsProxyEventStore struct {
	events []store.ProxyEvent
}

func (f *obsProxyEventStore) AppendProxyEvent(ev store.ProxyEvent) error {
	f.events = append(f.events, ev)
	return nil
}

func (f *obsProxyEventStore) QueryProxyEvents(filter store.ProxyEventFilter) ([]store.ProxyEvent, error) {
	var out []store.ProxyEvent
	for _, ev := range f.events {
		if filter.SessionID != "" && ev.SessionID != filter.SessionID {
			continue
		}
		if filter.ErrorsOnly && ev.OK {
			continue
		}
		if filter.ServerID != "" && ev.Server != filter.ServerID {
			continue
		}
		out = append(out, ev)
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ─── helper: build a full-featured observation test handler ──────────────────

func newObsHandler(
	svc LaunchService,
	evStore EventsStore,
	cpStore CheckpointStore,
	attStore AttachmentStore,
	proxyStore ProxyEventStore,
) http.Handler {
	return NewHandler(Deps{
		Service:     svc,
		EventsStore: evStore,
		Checkpoints: cpStore,
		Attachments: attStore,
		ProxyEvents: proxyStore,
	})
}

// ─── TestObservationContract ─────────────────────────────────────────────────

func TestObservationContract(t *testing.T) {
	// ── Scenario 1: Session lifecycle visibility ──────────────────────────────
	t.Run("S1_SessionLifecycleVisibility", func(t *testing.T) {
		sessionID := "obs-sess-001"
		now := "2026-04-23T10:00:00Z"

		svc := &fakeLaunchService{
			listRes: []store.SessionRow{
				{
					ID:        sessionID,
					State:     "running",
					CreatedAt: now,
					UpdatedAt: now,
					PID:       sql.NullInt64{Int64: 1234, Valid: true},
				},
			},
			getRes: map[string]*store.SessionRow{
				sessionID: {
					ID:        sessionID,
					State:     "running",
					CreatedAt: now,
					UpdatedAt: now,
				},
			},
			attachedClients: map[string]int{},
		}

		evStore := &obsEventsStore{
			byID: map[string][]events.Event{
				sessionID: {
					{Seq: 2, At: time.Unix(1745399002, 0).UTC(), Scope: events.ScopeSession, SessionID: sessionID, Kind: "session.state_changed"},
					{Seq: 1, At: time.Unix(1745399001, 0).UTC(), Scope: events.ScopeSession, SessionID: sessionID, Kind: "session.created"},
				},
			},
		}

		h := newObsHandler(svc, evStore, nil, nil, nil)

		// GET /sessions → session appears in list
		{
			req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /sessions status=%d, want 200: %s", rr.Code, rr.Body.String())
			}
			var resp ListSessionsResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode list response: %v", err)
			}
			if len(resp.Sessions) != 1 {
				t.Fatalf("sessions count=%d, want 1", len(resp.Sessions))
			}
			if resp.Sessions[0].ID != sessionID {
				t.Errorf("session id=%q, want %q", resp.Sessions[0].ID, sessionID)
			}
		}

		// GET /sessions/{id} → state=running, created_at set
		{
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID, nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /sessions/%s status=%d: %s", sessionID, rr.Code, rr.Body.String())
			}
			var dto SessionDTO
			if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
				t.Fatalf("decode session dto: %v", err)
			}
			if dto.State != "running" {
				t.Errorf("state=%q, want running", dto.State)
			}
			if dto.CreatedAt == "" {
				t.Error("created_at not set")
			}
		}

		// GET /sessions/{id}/events → 2 events returned, response shape matches
		{
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/events", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /sessions/%s/events status=%d: %s", sessionID, rr.Code, rr.Body.String())
			}
			var resp EventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode event list response: %v", err)
			}
			if len(resp.Events) != 2 {
				t.Fatalf("events count=%d, want 2", len(resp.Events))
			}
			// Verify response shape fields
			for i, ev := range resp.Events {
				if ev.Seq == 0 {
					t.Errorf("event[%d]: seq=0, want non-zero", i)
				}
				if ev.Kind == "" {
					t.Errorf("event[%d]: kind empty", i)
				}
				if ev.At == "" {
					t.Errorf("event[%d]: at empty", i)
				}
				if ev.SessionID != sessionID {
					t.Errorf("event[%d]: session_id=%q, want %q", i, ev.SessionID, sessionID)
				}
			}
		}
	})

	// ── Scenario 2: Terminal outcome visibility ───────────────────────────────
	t.Run("S2_TerminalOutcomeVisibility", func(t *testing.T) {
		sessionID := "obs-sess-002"
		exitCode := int64(0)

		svc := &fakeLaunchService{
			getRes: map[string]*store.SessionRow{
				sessionID: {
					ID:       sessionID,
					State:    "completed",
					ExitCode: sql.NullInt64{Int64: exitCode, Valid: true},
				},
			},
			attachedClients: map[string]int{},
		}

		h := newObsHandler(svc, nil, nil, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID, nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /sessions/%s status=%d: %s", sessionID, rr.Code, rr.Body.String())
		}
		var dto SessionDTO
		if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if dto.State != "completed" {
			t.Errorf("state=%q, want completed", dto.State)
		}
		if dto.ExitCode == nil {
			t.Fatal("exit_code missing from completed session response")
		}
		if *dto.ExitCode != int(exitCode) {
			t.Errorf("exit_code=%d, want %d", *dto.ExitCode, exitCode)
		}
	})

	// ── Scenario 3: Checkpoint visibility by session ──────────────────────────
	t.Run("S3_CheckpointVisibilityBySession", func(t *testing.T) {
		sessionID := "obs-sess-003"
		agentID := "obs-agent-003"

		svc := &fakeLaunchService{
			getRes: map[string]*store.SessionRow{
				sessionID: {
					ID:             sessionID,
					State:          "completed",
					LogicalAgentID: agentID,
				},
			},
			attachedClients: map[string]int{},
		}

		cpStore := &obsCheckpointStore{
			byAgent: map[string][]checkpoint.Checkpoint{
				agentID: {
					{
						ID:              "cp-2",
						LogicalAgentID:  agentID,
						Summary:         "newer checkpoint",
						CreatedAt:       "2026-04-23T10:05:00Z",
						SourceSessionID: sessionID,
					},
					{
						ID:              "cp-1",
						LogicalAgentID:  agentID,
						Summary:         "older checkpoint",
						CreatedAt:       "2026-04-23T10:00:00Z",
						SourceSessionID: sessionID,
					},
				},
			},
		}

		h := newObsHandler(svc, nil, cpStore, nil, nil)

		req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/checkpoints", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /sessions/%s/checkpoints status=%d: %s", sessionID, rr.Code, rr.Body.String())
		}
		var resp CheckpointListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Checkpoints) != 2 {
			t.Fatalf("checkpoints count=%d, want 2", len(resp.Checkpoints))
		}
		// Newest first
		if resp.Checkpoints[0].ID != "cp-2" {
			t.Errorf("checkpoints[0].id=%q, want cp-2 (newest first)", resp.Checkpoints[0].ID)
		}
		if resp.Checkpoints[1].ID != "cp-1" {
			t.Errorf("checkpoints[1].id=%q, want cp-1", resp.Checkpoints[1].ID)
		}
		// Verify checkpoint fields
		for i, cp := range resp.Checkpoints {
			if cp.LogicalAgentID != agentID {
				t.Errorf("checkpoints[%d].logical_agent_id=%q, want %q", i, cp.LogicalAgentID, agentID)
			}
			if cp.CreatedAt == "" {
				t.Errorf("checkpoints[%d].created_at empty", i)
			}
		}
	})

	// ── Scenario 4: Client attachment history ─────────────────────────────────
	t.Run("S4_ClientAttachmentHistory", func(t *testing.T) {
		sessionID := "obs-sess-004"
		attachedAt1 := "2026-04-23T09:00:00Z"
		detachedAt1 := "2026-04-23T09:30:00Z"
		attachedAt2 := "2026-04-23T10:00:00Z"

		svc := &fakeLaunchService{
			getRes:          map[string]*store.SessionRow{},
			attachedClients: map[string]int{},
		}

		attStore := &obsAttachmentStore{
			bySession: map[string][]store.ClientAttachmentRow{
				sessionID: {
					{
						ID:         "att-001",
						SessionID:  sessionID,
						ClientKind: "tui",
						AttachedAt: attachedAt1,
						DetachedAt: sql.NullString{String: detachedAt1, Valid: true},
					},
					{
						ID:         "att-002",
						SessionID:  sessionID,
						ClientKind: "tui",
						AttachedAt: attachedAt2,
						DetachedAt: sql.NullString{Valid: false}, // still active
					},
				},
			},
		}

		h := newObsHandler(svc, nil, nil, attStore, nil)

		req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/attachments", nil)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /sessions/%s/attachments status=%d: %s", sessionID, rr.Code, rr.Body.String())
		}
		var resp AttachmentListResponse
		if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Attachments) != 2 {
			t.Fatalf("attachments count=%d, want 2", len(resp.Attachments))
		}
		if resp.Count != 2 {
			t.Errorf("count=%d, want 2", resp.Count)
		}

		// First attachment: detached
		closed := resp.Attachments[0]
		if closed.ID != "att-001" {
			t.Errorf("attachment[0].id=%q, want att-001", closed.ID)
		}
		if closed.DetachedAt == "" {
			t.Error("attachment[0].detached_at should be set (was detached)")
		}
		if closed.DetachedAt != detachedAt1 {
			t.Errorf("attachment[0].detached_at=%q, want %q", closed.DetachedAt, detachedAt1)
		}

		// Second attachment: still active
		active := resp.Attachments[1]
		if active.ID != "att-002" {
			t.Errorf("attachment[1].id=%q, want att-002", active.ID)
		}
		if active.DetachedAt != "" {
			t.Errorf("attachment[1].detached_at=%q, want empty (still active)", active.DetachedAt)
		}
	})

	// ── Scenario 5: Proxy/tool call event visibility ──────────────────────────
	t.Run("S5_ProxyEventVisibility", func(t *testing.T) {
		ts := time.Now().UTC()

		proxyStore := &obsProxyEventStore{
			events: []store.ProxyEvent{
				{ID: 1, SessionID: "S1", Server: "hadron", ToolName: "hadron_task_list", OK: true, Timestamp: ts},
				{ID: 2, SessionID: "S1", Server: "hadron", ToolName: "hadron_task_create", OK: false, Error: "quota exceeded", Timestamp: ts},
				{ID: 3, SessionID: "S2", Server: "clockwork", ToolName: "clockwork_sprint_list", OK: true, Timestamp: ts},
			},
		}

		svc := &fakeLaunchService{
			getRes:          map[string]*store.SessionRow{},
			attachedClients: map[string]int{},
		}
		h := newObsHandler(svc, nil, nil, nil, proxyStore)

		// Filter by session_id: only S1 events
		{
			req := httptest.NewRequest(http.MethodGet, "/proxy/events?session_id=S1", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /proxy/events?session_id=S1 status=%d: %s", rr.Code, rr.Body.String())
			}
			var resp ProxyEventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Events) != 2 {
				t.Errorf("events count=%d, want 2 (S1 only)", len(resp.Events))
			}
			for _, ev := range resp.Events {
				if ev.SessionID != "S1" {
					t.Errorf("session_id=%q, want S1 (filter broken)", ev.SessionID)
				}
			}
		}

		// Filter errors_only=true: only failed events
		{
			req := httptest.NewRequest(http.MethodGet, "/proxy/events?errors_only=true", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /proxy/events?errors_only=true status=%d: %s", rr.Code, rr.Body.String())
			}
			var resp ProxyEventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Events) != 1 {
				t.Errorf("events count=%d, want 1 (errors only)", len(resp.Events))
			}
			if len(resp.Events) > 0 && resp.Events[0].OK {
				t.Error("errors_only filter returned an ok=true event")
			}
			if len(resp.Events) > 0 && resp.Events[0].Error == "" {
				t.Error("error field should be set on error event")
			}
		}

		// limit=2: max 2 returned out of 3
		{
			req := httptest.NewRequest(http.MethodGet, "/proxy/events?limit=2", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("GET /proxy/events?limit=2 status=%d: %s", rr.Code, rr.Body.String())
			}
			var resp ProxyEventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Events) != 2 {
				t.Errorf("events count=%d, want 2 (limit enforced)", len(resp.Events))
			}
			if resp.Count != 2 {
				t.Errorf("count field=%d, want 2", resp.Count)
			}
		}
	})

	// ── Scenario 6: Reconnect/replay simulation ───────────────────────────────
	t.Run("S6_ReconnectReplaySimulation", func(t *testing.T) {
		sessionID := "obs-sess-006"
		baseTime := time.Unix(1745400000, 0).UTC()

		evStore := &obsEventsStore{
			byID: map[string][]events.Event{
				sessionID: {
					{Seq: 5, At: baseTime.Add(4 * time.Second), Scope: events.ScopeSession, SessionID: sessionID, Kind: "k5"},
					{Seq: 4, At: baseTime.Add(3 * time.Second), Scope: events.ScopeSession, SessionID: sessionID, Kind: "k4"},
					{Seq: 3, At: baseTime.Add(2 * time.Second), Scope: events.ScopeSession, SessionID: sessionID, Kind: "k3"},
					{Seq: 2, At: baseTime.Add(1 * time.Second), Scope: events.ScopeSession, SessionID: sessionID, Kind: "k2"},
					{Seq: 1, At: baseTime, Scope: events.ScopeSession, SessionID: sessionID, Kind: "k1"},
				},
			},
		}

		svc := &fakeLaunchService{
			getRes:          map[string]*store.SessionRow{},
			attachedClients: map[string]int{},
		}
		h := newObsHandler(svc, evStore, nil, nil, nil)

		// Full history: no cursor → all 5 events (handler uses default limit=100)
		{
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/events", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("full history status=%d: %s", rr.Code, rr.Body.String())
			}
			var resp EventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Events) != 5 {
				t.Fatalf("full history: count=%d, want 5", len(resp.Events))
			}
		}

		// Paged replay: cursor=3 → events with seq < 3 → seqs 1 and 2
		{
			req := httptest.NewRequest(http.MethodGet, "/sessions/"+sessionID+"/events?cursor=3", nil)
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Fatalf("paged replay status=%d: %s", rr.Code, rr.Body.String())
			}
			var resp EventListResponse
			if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(resp.Events) != 2 {
				t.Fatalf("paged replay (cursor=3): count=%d, want 2 (seqs 1+2)", len(resp.Events))
			}
			// Verify only seqs 1 and 2 came back
			seqs := map[int64]bool{}
			for _, ev := range resp.Events {
				seqs[ev.Seq] = true
				if ev.Seq >= 3 {
					t.Errorf("event with seq=%d should have been filtered by cursor=3", ev.Seq)
				}
			}
			if !seqs[1] || !seqs[2] {
				t.Errorf("expected seqs 1+2 in paged result; got %v", seqs)
			}
		}
	})
}
