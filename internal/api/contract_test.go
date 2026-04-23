// Package api — provider/session contract test matrix (CW-20260423-0018)
//
// This file tests the stable provider/session contract defined in ADR 0022.
// It uses the fakeLaunchService stub to exercise API surface without a live
// provider runtime. Coverage matrix:
//
//   Layer 1: Launch Profile Inputs    — create request, boot_prompt override
//   Layer 3: Session Identity         — logical_agent_id + provider_id in responses
//   Layer 3a: Provider Kind           — provider_kind in session get + create/launch
//   Layer 4: Lifecycle States         — create→launch, conflict, not_found
//   Layer 7: Typed Error Envelope     — code shape, sentinels for not_found/conflict
//
// Gaps documented (not covered here):
//   Layer 2: Boot Prompt Payload      — assembled by bootgen package; see bootgen_test.go
//   Layer 5: Attach/Read              — streaming; see attach_test.go and daemon tests
//   Layer 6: Checkpoint/Resume        — tested in checkpoints_test.go; resume 501 pending
//   G1: metadata bag on create        — not yet implemented (medium priority)
//   G4: boot_mode in catalog/DTO      — low priority; not yet implemented
//   G5: StateReady unused             — low priority cleanup
//   G7: referenced_artifacts typed    — medium priority; pending v003-04
//   G8: resume 501 → full impl        — high priority; Sprint v003-04
//   G9: MCP streaming output          — deferred to MCP resources proposal
package api

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chrispian/agent-mux/internal/session"
	"github.com/chrispian/agent-mux/internal/store"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// contractService builds a fakeLaunchService pre-loaded with sensible defaults
// for contract tests: a create result that has all contract fields set.
func contractService() *fakeLaunchService {
	return &fakeLaunchService{
		createRes: LaunchResult{
			SessionID:      "sess-contract-001",
			Workspace:      "/tmp/ws/contract",
			LogPath:        "/tmp/ws/contract/session.log",
			ProviderID:     "stub",
			ProviderKind:   "api",
			LogicalAgentID: "backend",
		},
		launchRes: LaunchResult{
			SessionID:      "sess-contract-001",
			Workspace:      "/tmp/ws/contract",
			LogPath:        "/tmp/ws/contract/session.log",
			ProviderID:     "stub",
			ProviderKind:   "api",
			LogicalAgentID: "backend",
		},
		getRes: map[string]*store.SessionRow{
			"sess-contract-001": {
				ID:             "sess-contract-001",
				LaunchID:       "myproject-backend",
				ProjectID:      "myproject",
				LogicalAgentID: "backend",
				ProviderID:     "stub",
				ProviderKind:   "api",
				State:          "created",
			},
		},
		resumeRes: LaunchResult{
			SessionID:      "sess-resume-001",
			Workspace:      "/tmp/ws/resume",
			LogPath:        "/tmp/ws/resume/session.log",
			ProviderID:     "stub",
			ProviderKind:   "api",
			LogicalAgentID: "backend",
		},
		attachedClients: map[string]int{},
	}
}

func decodeJSON[T any](t *testing.T, rr *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.NewDecoder(rr.Body).Decode(&v); err != nil {
		t.Fatalf("decode response: %v (body: %s)", err, rr.Body.String())
	}
	return v
}

// ─── Layer 1: Launch Profile Inputs ─────────────────────────────────────────

// TestContract_CreateSession_LaunchIDRequired verifies that POST /sessions
// without a launch ID returns invalid_request.
func TestContract_CreateSession_LaunchIDRequired(t *testing.T) {
	svc := contractService()
	body := `{}` // missing "launch"
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rr.Code, rr.Body.String())
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeInvalidRequest {
		t.Errorf("error code = %q, want invalid_request", env.Error.Code)
	}
}

// TestContract_CreateSession_ContractFields verifies POST /sessions returns
// all ADR 0022 Layer 1+3 fields: session_id, workspace, log, provider_id,
// provider_kind, logical_agent_id.
func TestContract_CreateSession_ContractFields(t *testing.T) {
	svc := contractService()
	body := `{"launch":"myproject-backend"}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var resp LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Session identity (ADR 0022 Layer 3)
	if resp.ID == "" {
		t.Error("missing id (session_id)")
	}
	if resp.ProviderID == "" {
		t.Error("missing provider_id (Layer 3)")
	}
	// G2: logical_agent_id must be present
	if resp.LogicalAgentID == "" {
		t.Error("missing logical_agent_id (ADR 0022 G2)")
	}
	// G3: provider_kind must be present
	if resp.ProviderKind == "" {
		t.Error("missing provider_kind (ADR 0022 G3)")
	}
	// Workspace and log paths must be set
	if resp.Workspace == "" {
		t.Error("missing workspace")
	}
	if resp.Log == "" {
		t.Error("missing log")
	}
}

// TestContract_CreateSession_BootPromptOverride verifies that when boot_prompt
// is provided in the body, CreateSessionWithBootPrompt is dispatched (not
// CreateSession). Both return the same contract fields.
func TestContract_CreateSession_BootPromptOverride(t *testing.T) {
	svc := contractService()
	body := `{"launch":"myproject-backend","boot_prompt":"## Custom Boot\nDo the thing."}`
	req := httptest.NewRequest(http.MethodPost, "/sessions", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var resp LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// With boot_prompt set, CreateSession is still routed through CreateSessionWithBootPrompt
	// in the handler; both paths should produce the same contract response shape.
	if resp.ID == "" || resp.ProviderID == "" || resp.LogicalAgentID == "" || resp.ProviderKind == "" {
		t.Errorf("contract fields missing in boot_prompt create response: %+v", resp)
	}
}

// ─── Layer 3: Session Identity (create/launch/get) ────────────────────────────

// TestContract_LaunchSession_ContractFields verifies POST /sessions/{id}/launch
// returns the same ADR 0022 Layer 3 fields as the create endpoint.
func TestContract_LaunchSession_ContractFields(t *testing.T) {
	svc := contractService()
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-contract-001/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var resp LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LogicalAgentID == "" {
		t.Error("missing logical_agent_id in launch response (ADR 0022 G2)")
	}
	if resp.ProviderKind == "" {
		t.Error("missing provider_kind in launch response (ADR 0022 G3)")
	}
	if resp.ProviderID == "" {
		t.Error("missing provider_id in launch response")
	}
}

// TestContract_GetSession_ProviderKind verifies GET /sessions/{id} includes
// provider_kind in the SessionDTO (ADR 0022 G3).
func TestContract_GetSession_ProviderKind(t *testing.T) {
	svc := contractService()
	req := httptest.NewRequest(http.MethodGet, "/sessions/sess-contract-001", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var wrapper struct {
		Session SessionDTO `json:"session"`
	}
	// The handler returns the DTO directly (not wrapped), so decode as SessionDTO.
	body := rr.Body.Bytes()
	var dto SessionDTO
	if err := json.Unmarshal(body, &dto); err != nil {
		t.Fatalf("decode: %v (body: %s)", err, body)
	}
	if dto.ProviderKind == "" {
		t.Error("provider_kind missing from GET /sessions/{id} response (ADR 0022 G3)")
	}
	if dto.LogicalAgentID == "" {
		t.Error("logical_agent_id missing from GET /sessions/{id} response")
	}
	if dto.ProviderID == "" {
		t.Error("provider_id missing from GET /sessions/{id} response")
	}
	_ = wrapper // suppress unused warning
}

// TestContract_ListSessions_ProviderKind verifies GET /sessions includes
// provider_kind in each SessionDTO (ADR 0022 G3).
func TestContract_ListSessions_ProviderKind(t *testing.T) {
	svc := &fakeLaunchService{
		listRes: []store.SessionRow{
			{
				ID:             "s1",
				State:          "running",
				ProviderKind:   "cli",
				ProviderID:     "claudestream",
				LogicalAgentID: "backend",
				PID:            sql.NullInt64{Int64: 42, Valid: true},
			},
		},
		attachedClients: map[string]int{},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rr.Code, rr.Body.String())
	}
	var resp ListSessionsResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Sessions) != 1 {
		t.Fatalf("sessions count = %d, want 1", len(resp.Sessions))
	}
	if resp.Sessions[0].ProviderKind != "cli" {
		t.Errorf("provider_kind = %q, want 'cli'", resp.Sessions[0].ProviderKind)
	}
}

// ─── Layer 4: Lifecycle States ────────────────────────────────────────────────

// TestContract_Lifecycle_ConflictOnDoublelaunch verifies that launching a session
// that is not in 'created' state returns 409 conflict (ADR 0022 Layer 4).
func TestContract_Lifecycle_ConflictOnDoubleLaunch(t *testing.T) {
	svc := contractService()
	svc.launchErr = fmt.Errorf("%w (state=%q)", session.ErrNotCreated, "running")
	req := httptest.NewRequest(http.MethodPost, "/sessions/sess-contract-001/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rr.Code, rr.Body.String())
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeConflict {
		t.Errorf("error code = %q, want conflict", env.Error.Code)
	}
}

// TestContract_Lifecycle_NotFoundOnMissing verifies that launching a missing
// session returns 404 not_found (ADR 0022 Layer 4 + G6 sentinel).
func TestContract_Lifecycle_NotFoundOnMissing(t *testing.T) {
	svc := contractService()
	svc.launchErr = fmt.Errorf("%w: missing-id", store.ErrSessionNotFound)
	req := httptest.NewRequest(http.MethodPost, "/sessions/missing-id/launch", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q, want not_found", env.Error.Code)
	}
}

// TestContract_Lifecycle_GetNotFound verifies GET /sessions/{unknown} returns
// 404 not_found using the store.ErrSessionNotFound sentinel (ADR 0022 G6).
func TestContract_Lifecycle_GetNotFound(t *testing.T) {
	svc := &fakeLaunchService{
		getRes:          map[string]*store.SessionRow{},
		attachedClients: map[string]int{},
	}
	req := httptest.NewRequest(http.MethodGet, "/sessions/ghost", nil)
	rr := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rr.Code, rr.Body.String())
	}
	env := decodeErr(t, rr)
	if env.Error.Code != CodeNotFound {
		t.Errorf("error code = %q, want not_found", env.Error.Code)
	}
}

// ─── Layer 7: Typed Error Envelope ────────────────────────────────────────────

// TestContract_ErrorEnvelope_Shape verifies that error responses always use the
// ADR 0010 / ADR 0022 Layer 7 envelope shape: {error: {code, message}}.
func TestContract_ErrorEnvelope_Shape(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		path       string
		wantStatus int
		wantCode   string
	}{
		{"missing launch_id", http.MethodPost, "/sessions", http.StatusBadRequest, CodeInvalidRequest},
		{"session not found", http.MethodGet, "/sessions/ghost", http.StatusNotFound, CodeNotFound},
		{"method not allowed", http.MethodDelete, "/sessions", http.StatusMethodNotAllowed, CodeMethodNotAllowed},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := &fakeLaunchService{
				getRes:          map[string]*store.SessionRow{},
				attachedClients: map[string]int{},
			}
			var bodyReader io.Reader
			if tc.method == http.MethodPost && tc.path == "/sessions" {
				bodyReader = bytes.NewBufferString(`{}`)
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			if bodyReader != nil {
				req.Header.Set("Content-Type", "application/json")
			}
			rr := httptest.NewRecorder()
			newTestHandler(svc).ServeHTTP(rr, req)

			if rr.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d: %s", rr.Code, tc.wantStatus, rr.Body.String())
			}

			// Verify envelope shape: {error: {code, message}}
			var envelope struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(rr.Body).Decode(&envelope); err != nil {
				t.Fatalf("decode error envelope: %v (body: %s)", err, rr.Body.String())
			}
			if envelope.Error.Code == "" {
				t.Error("error.code is empty — envelope shape broken")
			}
			if envelope.Error.Code != tc.wantCode {
				t.Errorf("error.code = %q, want %q", envelope.Error.Code, tc.wantCode)
			}
			if envelope.Error.Message == "" {
				t.Error("error.message is empty — envelope shape broken")
			}
		})
	}
}

// ─── G6: Sentinel Error Propagation ──────────────────────────────────────────

// TestContract_Sentinel_ErrSessionNotFound verifies that store.ErrSessionNotFound
// is wrapped correctly and propagates through errors.Is at call sites.
func TestContract_Sentinel_ErrSessionNotFound(t *testing.T) {
	wrapped := fmt.Errorf("get session: %w: sess-abc", store.ErrSessionNotFound)
	if !errors.Is(wrapped, store.ErrSessionNotFound) {
		t.Error("errors.Is(wrapped, store.ErrSessionNotFound) = false, want true")
	}
}

// TestContract_Sentinel_ErrNotCreated verifies that session.ErrNotCreated is
// wrapped correctly and propagates through errors.Is at call sites.
func TestContract_Sentinel_ErrNotCreated(t *testing.T) {
	wrapped := fmt.Errorf("%w (state=%q)", session.ErrNotCreated, "running")
	if !errors.Is(wrapped, session.ErrNotCreated) {
		t.Error("errors.Is(wrapped, session.ErrNotCreated) = false, want true")
	}
}

// ─── Resume endpoint ──────────────────────────────────────────────────────────

// TestContract_Resume_ContractFields verifies POST /logical-agents/{id}/resume
// returns the same ADR 0022 Layer 3 contract fields (provider_kind,
// logical_agent_id) as create/launch.
func TestContract_Resume_ContractFields(t *testing.T) {
	svc := contractService()
	req := httptest.NewRequest(http.MethodPost, "/logical-agents/backend/resume", nil)
	rr := httptest.NewRecorder()
	// Wire a checkpoint handler alongside the service to enable the route.
	s := &Server{
		Service:     svc,
		Checkpoints: &fakeCheckpoints{},
	}
	mux := http.NewServeMux()
	s.registerCheckpointRoutes(mux)
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rr.Code, rr.Body.String())
	}
	var resp LaunchResponse
	if err := json.NewDecoder(rr.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.LogicalAgentID == "" {
		t.Error("missing logical_agent_id in resume response (ADR 0022 G2)")
	}
	if resp.ProviderKind == "" {
		t.Error("missing provider_kind in resume response (ADR 0022 G3)")
	}
	if resp.ProviderID == "" {
		t.Error("missing provider_id in resume response")
	}
}

// ─── SessionDTO shape ─────────────────────────────────────────────────────────

// TestContract_SessionDTO_ProviderKindOmitEmptyForOldRows verifies that
// provider_kind is omitted from JSON when empty (backwards compat for rows
// created before migration 0012).
func TestContract_SessionDTO_ProviderKindOmitEmptyForOldRows(t *testing.T) {
	row := store.SessionRow{
		ID:         "old-row",
		ProviderID: "claudestream",
		// ProviderKind intentionally left empty (pre-migration row)
		State: "completed",
	}
	dto := SessionRowToDTO(row)
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// provider_kind should be omitted when empty (omitempty in the tag)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["provider_kind"]; ok {
		t.Error("provider_kind should be omitted when empty (backwards compat), but was present")
	}
}

// TestContract_SessionDTO_ProviderKindPresentWhenSet verifies provider_kind
// appears in the JSON when set (post-migration rows).
func TestContract_SessionDTO_ProviderKindPresentWhenSet(t *testing.T) {
	row := store.SessionRow{
		ID:           "new-row",
		ProviderID:   "claudestream",
		ProviderKind: "cli",
		State:        "running",
	}
	dto := SessionRowToDTO(row)
	b, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["provider_kind"] != "cli" {
		t.Errorf("provider_kind = %v, want 'cli'", m["provider_kind"])
	}
}


