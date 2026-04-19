package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chrispian/agent-mux/internal/checkpoint"
	"github.com/chrispian/agent-mux/internal/store"
)

// fakeCheckpoints records calls for assertion and lets tests override
// both create and list paths.
type fakeCheckpoints struct {
	created []checkpoint.Checkpoint
	listRes map[string][]checkpoint.Checkpoint
	listErr error
}

func (f *fakeCheckpoints) CreateCheckpoint(c checkpoint.Checkpoint) error {
	f.created = append(f.created, c)
	return nil
}

func (f *fakeCheckpoints) ListCheckpointsByLogicalAgent(agentID string) ([]checkpoint.Checkpoint, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.listRes[agentID], nil
}

func newCheckpointTestHandler(svc LaunchService, cp CheckpointStore) http.Handler {
	return NewHandler(Deps{Service: svc, Checkpoints: cp})
}

func TestHandleCreateCheckpoint_Success(t *testing.T) {
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{
		"sess-1": {ID: "sess-1", LogicalAgentID: "agent-1"},
	}}
	cp := &fakeCheckpoints{}
	body, _ := json.Marshal(CheckpointCreateRequest{
		Summary:       "stopped at T-05",
		CompletedWork: "tasks 1-4",
	})
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/checkpoint", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var dto CheckpointDTO
	if err := json.NewDecoder(rr.Body).Decode(&dto); err != nil {
		t.Fatal(err)
	}
	if dto.ID == "" {
		t.Error("id missing on response")
	}
	if dto.LogicalAgentID != "agent-1" {
		t.Errorf("logical_agent_id = %q, want agent-1", dto.LogicalAgentID)
	}
	if dto.SourceSessionID != "sess-1" {
		t.Errorf("source_session_id = %q, want sess-1", dto.SourceSessionID)
	}
	if dto.Summary != "stopped at T-05" {
		t.Errorf("summary not preserved: %q", dto.Summary)
	}
	if dto.CreatedAt == "" {
		t.Error("created_at missing")
	}

	if len(cp.created) != 1 {
		t.Fatalf("persisted %d checkpoints, want 1", len(cp.created))
	}
	persisted := cp.created[0]
	if persisted.ID != dto.ID || persisted.LogicalAgentID != "agent-1" {
		t.Errorf("persisted mismatch: %+v", persisted)
	}
}

func TestHandleCreateCheckpoint_EmptyBody(t *testing.T) {
	// No body — v0.0.2 accepts it and creates a minimal row.
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{
		"sess-1": {ID: "sess-1", LogicalAgentID: "agent-1"},
	}}
	cp := &fakeCheckpoints{}
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/checkpoint", nil)
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d", rr.Code)
	}
	if len(cp.created) != 1 {
		t.Fatalf("expected one checkpoint persisted; got %d", len(cp.created))
	}
}

func TestHandleCreateCheckpoint_BadJSON(t *testing.T) {
	svc := &fakeLaunchService{}
	cp := &fakeCheckpoints{}
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-1/checkpoint", bytes.NewReader([]byte("{")))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("code = %q", env.Error.Code)
	}
}

func TestHandleCreateCheckpoint_SessionNotFound(t *testing.T) {
	svc := &fakeLaunchService{getRes: map[string]*store.SessionRow{}}
	cp := &fakeCheckpoints{}
	req := httptest.NewRequest(http.MethodPost, "/sessions/missing/checkpoint", nil)
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rr.Code)
	}
}

func TestHandleListCheckpoints(t *testing.T) {
	svc := &fakeLaunchService{}
	cp := &fakeCheckpoints{
		listRes: map[string][]checkpoint.Checkpoint{
			"agent-1": {
				{ID: "cp-2", LogicalAgentID: "agent-1", Summary: "newer", CreatedAt: "2026-04-19T10:05:00Z"},
				{ID: "cp-1", LogicalAgentID: "agent-1", Summary: "older", CreatedAt: "2026-04-19T10:00:00Z"},
			},
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/logical-agents/agent-1/checkpoints", nil)
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d", rr.Code)
	}
	var res CheckpointListResponse
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatal(err)
	}
	if len(res.Checkpoints) != 2 || res.Checkpoints[0].ID != "cp-2" {
		t.Errorf("unexpected list: %+v", res.Checkpoints)
	}
}

func TestHandleResumeLogicalAgent_Returns501(t *testing.T) {
	svc := &fakeLaunchService{}
	cp := &fakeCheckpoints{}
	req := httptest.NewRequest(http.MethodPost, "/logical-agents/agent-1/resume", nil)
	rr := httptest.NewRecorder()
	newCheckpointTestHandler(svc, cp).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", rr.Code)
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotImplemented {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeNotImplemented)
	}
}

func TestCheckpointRoutes_NotRegisteredWithoutStore(t *testing.T) {
	svc := &fakeLaunchService{}
	// No checkpoints → routes absent → 404.
	h := NewHandler(Deps{Service: svc})

	req := httptest.NewRequest(http.MethodGet, "/logical-agents/a/checkpoints", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404 when Checkpoints nil; got %d", rr.Code)
	}

	// /sessions/{id}/checkpoint should also 404 with a typed error.
	req = httptest.NewRequest(http.MethodPost, "/sessions/s1/checkpoint", nil)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Errorf("want 404 when Checkpoints nil; got %d", rr.Code)
	}
}
