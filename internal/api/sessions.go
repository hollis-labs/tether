package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// registerSessionRoutes wires /sessions handlers onto router. Route matching
// is hand-rolled: two static collection paths, a few parameterised item
// actions. Introducing a third-party router would be overkill at this
// size.
func (s *Server) registerSessionRoutes(router *http.ServeMux) {
	if s.Service == nil {
		return
	}
	router.HandleFunc("/sessions", s.handleSessionsCollection)
	router.HandleFunc("/sessions/", s.handleSessionsItem)
}

func (s *Server) handleSessionsCollection(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleListSessions(w, r)
	case http.MethodPost:
		s.handleLaunch(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleSessionsItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/sessions/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "session id required")
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}

	switch action {
	case "":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleGetSession(w, r, id)
	case "launch":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleLaunchSession(w, r, id)
	case "stop":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleStopSession(w, r, id)
	case "wait":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleWaitSession(w, r, id)
	case "input":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSendInput(w, r, id)
	case "turn":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSendTurn(w, r, id)
	case "resize":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleResizeSession(w, r, id)
	case "log":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSessionLog(w, r, id)
	case "attach":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleAttach(w, r, id)
	case "checkpoint":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.Checkpoints == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "checkpoints not configured")
			return
		}
		s.handleCreateCheckpoint(w, r, id)
	case "events":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.EventsStore == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "events store not configured")
			return
		}
		s.handleSessionEventsList(w, r, id)
	case "attachments":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.Attachments == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "attachments store not configured")
			return
		}
		s.handleSessionAttachmentsList(w, r, id)
	case "checkpoints":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		if s.Checkpoints == nil {
			writeError(w, http.StatusNotFound, CodeNotFound, "checkpoints not configured")
			return
		}
		s.handleSessionCheckpointsList(w, r, id)
	case "health":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSessionHealth(w, r, id)
	case "workstream":
		if r.Method != http.MethodPost {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSessionWorkstream(w, r, id)
	case "refs":
		s.handleSessionRefs(w, r, id)
	case "workstream-namespace":
		if r.Method != http.MethodGet {
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
			return
		}
		s.handleSessionWorkstreamNamespace(w, r, id)
	case "digest":
		s.handleSessionDigest(w, r, id)
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown action "+action)
	}
}

// handleSessionHealth services GET /sessions/{id}/health.
// Returns the live runtime health snapshot for a running session, including
// provider identity, capability flags, and fine-grained live state.
//
// Returns 404 if the session does not exist in the store, 409 if the session
// is not currently registered in the runtime manager (not in running state),
// or 200 with the RuntimeHealthResponse on success.
func (s *Server) handleSessionHealth(w http.ResponseWriter, _ *http.Request, id string) {
	// Confirm the session exists in the store first.
	if _, err := s.Service.GetSession(id); err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	// Read live runtime health from the manager.
	result, ok := s.Service.RuntimeHealth(id)
	if !ok {
		writeError(w, http.StatusConflict, CodeConflict, "session is not currently running; no live health available")
		return
	}
	caps := result.Caps
	writeJSON(w, http.StatusOK, RuntimeHealthResponse{
		SessionID:    id,
		Alive:        result.Health.Alive,
		PID:          result.Health.PID,
		LiveState:    result.Health.State.String(),
		TurnID:       result.Health.TurnID,
		ProviderID:   result.ProviderID,
		ProviderKind: result.ProviderKind,
		Caps: CapabilitiesDTO{
			PTY:               caps.PTY,
			StreamingStdio:    caps.StreamingStdio,
			JSONRPCStdio:      caps.JsonRpcStdio,
			Resize:            caps.Resize,
			ProviderSessionID: caps.ProviderSessionID,
			CheckpointResume:  caps.CheckpointResume,
			BinaryRequired:    caps.BinaryRequired,
		},
	})
}

// handleLaunch services POST /sessions — the create leg of the split.
// Prepares workspace + persists the session row in state=created, but
// does NOT start the runtime. Callers subsequently POST to
// /sessions/{id}/launch to start. Returns 201.
func (s *Server) handleLaunch(w http.ResponseWriter, r *http.Request) {
	var req LaunchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+remoteResponseError(r, err, "session creation failed").Error())
		return
	}
	if req.Launch == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "launch id required")
		return
	}
	if msg := validateIdempotencyKey(req.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, msg)
		return
	}
	key, keyErr := principalIdempotencyKey(r.Context(), req.IdempotencyKey)
	if keyErr != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "verified device required")
		return
	}
	req.IdempotencyKey = key
	var res LaunchResult
	var err error
	// A keyed request always takes the input path: that is where the
	// request digest is computed and the key recorded.
	if req.Route != nil || req.IdempotencyKey != "" || req.AgentFile != "" || req.AgentInline != "" || req.BootProfileFile != "" || req.Override != "" || req.Injection != "" || req.PromptAppend != "" {
		// v005-08 Tier-2 path: caller-provided payload (any field set routes here).
		res, err = s.Service.CreateSessionWithInput(CreateSessionInput{
			LaunchID:           req.Launch,
			Route:              req.Route,
			BootPromptOverride: req.BootPrompt,
			AgentFile:          req.AgentFile,
			AgentInline:        req.AgentInline,
			BootProfileFile:    req.BootProfileFile,
			Override:           req.Override,
			PromptAppend:       req.PromptAppend,
			Injection:          req.Injection,
			IdempotencyKey:     req.IdempotencyKey,
		})
	} else if req.BootPrompt != "" {
		res, err = s.Service.CreateSessionWithBootPrompt(req.Launch, req.BootPrompt)
	} else {
		res, err = s.Service.CreateSession(req.Launch)
	}
	if err != nil {
		var invalidRoute *launchprofile.InvalidRouteError
		if errors.As(err, &invalidRoute) {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, remoteResponseError(r, err, "session creation failed").Error())
			return
		}
		if writeIdempotencyConflict(w, remoteResponseError(r, err, "session request conflicts with prior request")) {
			return
		}
		if writeLaunchRefused(w, remoteResponseError(r, err, "session launch refused")) {
			return
		}
		if errors.Is(err, launch.ErrLaunchNotFound) || errors.Is(err, config.ErrUnknownSandboxProfile) {
			writeError(w, http.StatusNotFound, CodeNotFound, remoteResponseError(r, err, "session creation failed").Error())
			return
		}
		if errors.Is(err, config.ErrSandboxOverrideRefused) {
			writeError(w, http.StatusForbidden, CodeForbidden, remoteResponseError(r, err, "session creation failed").Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, remoteResponseError(r, err, "session creation failed").Error())
		return
	}
	status := http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	writeJSON(w, status, launchResponseFor(r, res))
}

// launchResponseOf renders a LaunchResult as the wire response.
func launchResponseOf(res LaunchResult) LaunchResponse {
	return LaunchResponse{
		ID:             res.SessionID,
		Workspace:      res.Workspace,
		Log:            res.LogPath,
		ProviderID:     res.ProviderID,
		ProviderKind:   res.ProviderKind,
		LogicalAgentID: res.LogicalAgentID,
		Replayed:       res.Replayed,
	}
}

// maxIdempotencyKeyBytes bounds a caller's idempotency key.
const maxIdempotencyKeyBytes = 512

// validateIdempotencyKey returns a reason an idempotency key is unusable, or
// "" when it is absent or acceptable.
func validateIdempotencyKey(key string) string {
	if key == "" {
		return ""
	}
	if strings.TrimSpace(key) != key {
		return "idempotency_key must not have leading or trailing whitespace"
	}
	if len(key) > maxIdempotencyKeyBytes {
		return fmt.Sprintf("idempotency_key exceeds %d bytes", maxIdempotencyKeyBytes)
	}
	for _, r := range key {
		if unicode.IsControl(r) {
			return "idempotency_key must not contain control characters"
		}
	}
	return ""
}

// writeIdempotencyConflict answers a key reused with a different request.
func writeIdempotencyConflict(w http.ResponseWriter, err error) bool {
	if !errors.Is(err, store.ErrIdempotencyConflict) {
		return false
	}
	writeError(w, http.StatusConflict, CodeIdempotencyConflict, err.Error())
	return true
}

// handleLaunchSession services POST /sessions/{id}/launch — the start
// leg of the split. Returns 200 on successful transition to running,
// 409 when the session is not in 'created' state, 404 when not found.
func (s *Server) handleLaunchSession(w http.ResponseWriter, r *http.Request, id string) {
	var res LaunchResult
	var err error
	if contextual, ok := s.Service.(interface {
		LaunchSessionWithContext(context.Context, string) (LaunchResult, error)
	}); ok {
		res, err = contextual.LaunchSessionWithContext(r.Context(), id)
	} else {
		res, err = s.Service.LaunchSession(id)
	}
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		if writeLaunchRefused(w, remoteResponseError(r, err, "session launch refused")) {
			return
		}
		if errors.Is(err, session.ErrNotCreated) {
			writeError(w, http.StatusConflict, CodeConflict, remoteResponseError(r, err, "session start failed").Error())
			return
		}
		if errors.Is(err, config.ErrUnknownSandboxProfile) {
			writeError(w, http.StatusNotFound, CodeNotFound, remoteResponseError(r, err, "session start failed").Error())
			return
		}
		if errors.Is(err, config.ErrSandboxOverrideRefused) {
			writeError(w, http.StatusForbidden, CodeForbidden, remoteResponseError(r, err, "session start failed").Error())
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, remoteResponseError(r, err, "session start failed").Error())
		return
	}
	writeJSON(w, http.StatusOK, launchResponseFor(r, res))
}

// handleListSessions services GET /sessions. Supports three query
// params: ?limit= (1..1000, default 100), ?cursor= (RFC3339, returns
// rows strictly older than this timestamp), ?state= (filters to a
// single session state). Malformed params yield 400 so callers know
// to fix them rather than silently paging the full set.
func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	opts, err := parseListSessionsOpts(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, remoteResponseError(r, err, "session listing failed").Error())
		return
	}

	rows, err := s.Service.ListSessions(opts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, remoteResponseError(r, err, "session listing failed").Error())
		return
	}
	out := make([]SessionDTO, 0, len(rows))
	for _, row := range rows {
		dto := SessionRowToDTO(row)
		dto.AttachedClients = s.Service.AttachedClients(row.ID)
		out = append(out, sessionResponseFor(r, dto))
	}

	// When we hit the requested page size, more rows may exist older
	// than the last returned row's created_at — hand that back as the
	// next cursor. When we returned fewer than the limit, we know we
	// reached the end and emit no cursor.
	resp := ListSessionsResponse{Sessions: out}
	effectiveLimit := opts.Limit
	if effectiveLimit <= 0 {
		effectiveLimit = 100
	}
	if len(rows) == effectiveLimit && len(rows) > 0 {
		resp.NextCursor = rows[len(rows)-1].CreatedAt
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseListSessionsOpts extracts pagination + filter params from the
// request URL. Returns a typed error message for malformed values; the
// caller maps it to 400.
func parseListSessionsOpts(r *http.Request) (store.ListSessionsOptions, error) {
	q := r.URL.Query()
	opts := store.ListSessionsOptions{
		Cursor: q.Get("cursor"),
		State:  q.Get("state"),
	}
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil {
			return opts, errors.New("invalid limit: must be integer")
		}
		if n < 0 {
			return opts, errors.New("invalid limit: must be >= 0")
		}
		opts.Limit = n
	}
	return opts, nil
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request, id string) {
	row, err := s.Service.GetSession(id)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, remoteResponseError(r, err, "session lookup failed").Error())
		return
	}
	dto := SessionRowToDTO(*row)
	dto.AttachedClients = s.Service.AttachedClients(id)
	writeJSON(w, http.StatusOK, sessionResponseFor(r, dto))
}

func (s *Server) handleStopSession(w http.ResponseWriter, _ *http.Request, id string) {
	if err := s.Service.StopSession(id); err != nil {
		var coded interface{ ErrorCode() string }
		if errors.As(err, &coded) {
			writeError(w, http.StatusConflict, coded.ErrorCode(), err.Error())
			return
		}
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleResizeSession services POST /sessions/{id}/resize. Body is a
// ResizeRequest with rows + cols (both > 0). On success returns 204.
// Typed errors: invalid_request (missing/zero fields), not_found
// (session id unknown OR not currently running — the underlying
// runtime distinguishes only ErrSessionNotRunning).
func (s *Server) handleResizeSession(w http.ResponseWriter, r *http.Request, id string) {
	var req ResizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Rows == 0 || req.Cols == 0 {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "rows and cols must be > 0")
		return
	}
	if err := s.Service.ResizeSession(id, req.Rows, req.Cols); err != nil {
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleWaitSession(w http.ResponseWriter, r *http.Request, id string) {
	code, err := s.Service.WaitSession(r.Context(), id)
	if err != nil {
		if errors.Is(err, agentsessions.ErrSessionNotRunning) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not running")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, WaitResponse{ExitCode: code})
}

// handleSessionAttachmentsList services GET /sessions/{id}/attachments.
// Returns the attach/detach history for the session in attached_at order
// (oldest first). DetachedAt is emitted as "" when the attachment is still
// open or the row predates detach tracking.
func (s *Server) handleSessionAttachmentsList(w http.ResponseWriter, _ *http.Request, sessionID string) {
	rows, err := s.Attachments.ListClientAttachments(sessionID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]AttachmentDTO, 0, len(rows))
	for _, r := range rows {
		detachedAt := ""
		if r.DetachedAt.Valid {
			detachedAt = r.DetachedAt.String
		}
		out = append(out, AttachmentDTO{
			ID:         r.ID,
			SessionID:  r.SessionID,
			ClientKind: r.ClientKind,
			AttachedAt: r.AttachedAt,
			DetachedAt: detachedAt,
		})
	}
	writeJSON(w, http.StatusOK, AttachmentListResponse{Attachments: out, Count: len(out)})
}

// handleSessionCheckpointsList services GET /sessions/{id}/checkpoints.
// Resolves the session's logical agent ID and returns that agent's checkpoints.
func (s *Server) handleSessionCheckpointsList(w http.ResponseWriter, _ *http.Request, sessionID string) {
	row, err := s.Service.GetSession(sessionID)
	if err != nil {
		if errors.Is(err, store.ErrSessionNotFound) {
			writeError(w, http.StatusNotFound, CodeNotFound, "session not found")
			return
		}
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	cps, err := s.Checkpoints.ListCheckpointsByLogicalAgent(row.LogicalAgentID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
		return
	}
	out := make([]CheckpointDTO, 0, len(cps))
	for _, c := range cps {
		out = append(out, checkpointToDTO(c))
	}
	writeJSON(w, http.StatusOK, CheckpointListResponse{Checkpoints: out})
}

// writeLaunchRefused answers a launch the daemon refuses by policy, with
// 403: the request is well formed, and the daemon will not run it until its
// configuration changes. The refusals are an ACP-mode launch while Tether
// write-protects its directories, which the ACP launcher cannot enforce yet
// (launch.ErrACPLaunchUnprotected, CW-20261001-0162), and a launch whose
// agent would work inside a protected directory
// (launch.ErrLaunchInsideProtectedPath), or any launch while protection is on
// and the host cannot provide it (launch.ErrProtectionUnavailable: bubblewrap
// is missing; CW-20261001-0142). A launch refused because a project's catalog layer
// cannot be protected (launch.ErrProjectLayerUnprotectable) is 403 too but with its
// own code, since the fix is that project's catalog entry and not the host.
func writeLaunchRefused(w http.ResponseWriter, err error) bool {
	var coded interface{ ErrorCode() string }
	if errors.As(err, &coded) {
		writeError(w, http.StatusConflict, coded.ErrorCode(), err.Error())
		return true
	}
	if errors.Is(err, launch.ErrLaunchProjectRootMissing) {
		writeError(w, http.StatusConflict, CodeProjectRootMissing, err.Error())
		return true
	}
	if errors.Is(err, launch.ErrProjectLayerUnprotectable) {
		writeError(w, http.StatusForbidden, CodeProjectLayerUnprotectable, err.Error())
		return true
	}
	if !errors.Is(err, launch.ErrACPLaunchUnprotected) &&
		!errors.Is(err, launch.ErrLaunchInsideProtectedPath) &&
		!errors.Is(err, launch.ErrProtectionUnavailable) {
		return false
	}
	writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	return true
}
