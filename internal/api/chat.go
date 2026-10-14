package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/hollis-labs/tether/internal/workerhub"
)

// CodeAgentWorking (409) means the chat agent's session is mid-turn, so the
// message was not sent and the caller keeps its draft.
const CodeAgentWorking = "agent_working"

// CodeWorkerNotConfigured (409) means there is no running session to continue
// and the launch declares no `worker:`. The hub never runs agents itself.
const CodeWorkerNotConfigured = "worker_not_configured"

// CodeWorkerUnavailable (502) means the launch's worker is not enrolled,
// offline, or the worker channel is down.
const CodeWorkerUnavailable = "worker_unavailable"

const (
	chatSendPrefix  = "/chat/agents/"
	chatSendSuffix  = "/send"
	chatMaxTextSize = 64 * 1024
	// chatReplayTTL bounds how long a delivered idempotency key is remembered
	// for the running-session path. The no-session path rides the durable
	// session idempotency key instead.
	chatReplayTTL = 24 * time.Hour
	chatReplayMax = 4096
)

// ChatAgent is one row of the operator chat roster.
type ChatAgent struct {
	LaunchID    string `json:"launch_id"`
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
	Channel     string `json:"channel"`
	Worker      string `json:"worker,omitempty"`
	// Enabled is false when the launch or its agent is disabled. The roster
	// omits disabled entries unless ?include_disabled=true.
	Enabled bool         `json:"enabled"`
	Session *ChatSession `json:"session"`
}

// ChatSession is the newest session of a roster launch. AtTurnBoundary is
// true only for a running session that is waiting for input.
type ChatSession struct {
	ID             string `json:"id"`
	State          string `json:"state"`
	LastActivityAt string `json:"last_activity_at"`
	AtTurnBoundary bool   `json:"at_turn_boundary"`
}

// ListChatAgentsResponse is the body of GET /chat/agents.
type ListChatAgentsResponse struct {
	Agents []ChatAgent `json:"agents"`
}

// ChatSendRequest is the body of POST /chat/agents/{launch_id}/send.
type ChatSendRequest struct {
	Text           string `json:"text"`
	IdempotencyKey string `json:"idempotency_key"`
}

// ChatSendResponse reports how the text was delivered: "turn" to a running
// session, or "launch" as the first prompt of a new one.
type ChatSendResponse struct {
	LaunchID  string `json:"launch_id"`
	SessionID string `json:"session_id"`
	Mode      string `json:"mode"`
	Replayed  bool   `json:"replayed"`
}

// chatReplays remembers delivered running-session sends so a retry with the
// same key does not deliver twice. In memory: a daemon restart forgets it.
type chatReplays struct {
	mu      sync.Mutex
	entries map[string]chatReplay
}

type chatReplay struct {
	digest string
	done   bool
	resp   ChatSendResponse
	at     time.Time
}

func (s *Server) registerChatRoutes(router *http.ServeMux) {
	if s.Service == nil || s.Catalog == nil {
		return
	}
	router.HandleFunc("/chat/agents", s.handleListChatAgents)
	router.HandleFunc("/chat/agents/", s.handleChatAgentItem)
}

func (s *Server) handleListChatAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	ids := make([]string, 0)
	includeDisabled := r.URL.Query().Get("include_disabled") == "true" || r.URL.Query().Get("include_disabled") == "1"
	for id, l := range cat.Launches {
		if l.Chat && (includeDisabled || chatEnabled(cat, l)) {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	out := make([]ChatAgent, 0, len(ids))
	for _, id := range ids {
		l := cat.Launches[id]
		row, err := s.newestLaunchSession(id)
		if err != nil {
			writeError(w, http.StatusInternalServerError, CodeInternalError, "session lookup failed")
			return
		}
		out = append(out, ChatAgent{
			LaunchID:    id,
			AgentID:     l.Agent,
			DisplayName: chatDisplayName(l),
			Channel:     chatChannel(cat, l),
			Worker:      l.Worker,
			Enabled:     chatEnabled(cat, l),
			Session:     s.chatSession(row),
		})
	}
	writeJSON(w, http.StatusOK, ListChatAgentsResponse{Agents: out})
}

func chatDisplayName(l config.Launch) string {
	if l.DisplayName != "" {
		return l.DisplayName
	}
	return l.Agent
}

// chatChannel is the channel the launch routes to: its own route, else its
// agent's. Empty when neither routes.
func chatChannel(cat *config.Catalog, l config.Launch) string {
	if l.Route != nil {
		return l.Route.Channel
	}
	if a, ok := cat.Agents[l.Agent]; ok && a.Route != nil {
		return a.Route.Channel
	}
	return ""
}

func (s *Server) newestLaunchSession(launchID string) (*store.SessionRow, error) {
	rows, err := s.Service.ListSessions(store.ListSessionsOptions{LaunchID: launchID, Limit: 1})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	// A fake or older store may ignore the filter; never trust it blindly.
	for i := range rows {
		if rows[i].LaunchID == launchID {
			return &rows[i], nil
		}
	}
	return nil, nil
}

func (s *Server) chatSession(row *store.SessionRow) *ChatSession {
	if row == nil {
		return nil
	}
	out := &ChatSession{ID: row.ID, State: row.State, LastActivityAt: row.UpdatedAt}
	if row.State == string(agentsessions.StateRunning) {
		if h, ok := s.Service.RuntimeHealth(row.ID); ok && h.Health.State == agentsessions.LiveStateIdle {
			out.AtTurnBoundary = true
		}
	}
	return out
}

func (s *Server) handleChatAgentItem(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, chatSendPrefix)
	launchID, ok := strings.CutSuffix(rest, chatSendSuffix)
	if !ok || launchID == "" || strings.Contains(launchID, "/") {
		writeError(w, http.StatusNotFound, CodeNotFound, "not found")
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed, "method not allowed")
		return
	}
	s.handleChatSend(w, r, launchID)
}

func (s *Server) handleChatSend(w http.ResponseWriter, r *http.Request, launchID string) {
	var req ChatSendRequest
	dec := json.NewDecoder(io.LimitReader(r.Body, chatMaxTextSize+4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "decode body: "+err.Error())
		return
	}
	if strings.TrimSpace(req.Text) == "" || !utf8.ValidString(req.Text) {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "text required")
		return
	}
	if len(req.Text) > chatMaxTextSize {
		writeError(w, http.StatusRequestEntityTooLarge, CodePayloadTooLarge, "text too large")
		return
	}
	if req.IdempotencyKey == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "idempotency_key required")
		return
	}
	if msg := validateIdempotencyKey(req.IdempotencyKey); msg != "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, msg)
		return
	}
	principal, err := idempotencyPrincipal(r.Context())
	if err != nil {
		writeError(w, http.StatusUnauthorized, "unauthorized", "verified device required")
		return
	}
	cat, ok := s.loadCatalog(w)
	if !ok {
		return
	}
	l, found := cat.Launches[launchID]
	if !found || !l.Chat {
		writeError(w, http.StatusNotFound, CodeNotFound, "chat agent not found")
		return
	}
	if !chatEnabled(cat, l) {
		writeError(w, http.StatusConflict, CodeAgentDisabled, "chat agent "+launchID+" is disabled (enabled: false); sending is blocked, a running session is untouched")
		return
	}

	sum := sha256.Sum256([]byte(launchID + "\x00" + req.Text))
	digest := hex.EncodeToString(sum[:])
	replayKey := principal + "\x00" + launchID + "\x00" + req.IdempotencyKey
	if resp, done, conflict := s.chatReplays.begin(replayKey, digest); conflict {
		writeError(w, http.StatusConflict, CodeIdempotencyConflict, "idempotency_key reused with a different message")
		return
	} else if done {
		resp.Replayed = true
		writeJSON(w, http.StatusOK, resp)
		return
	}

	resp, status, code, msg := s.deliverChat(r, launchID, l.Worker, req, principal)
	if code != "" {
		s.chatReplays.abandon(replayKey)
		writeError(w, status, code, msg)
		return
	}
	s.chatReplays.finish(replayKey, resp)
	writeJSON(w, status, resp)
}

// deliverChat sends to the newest running session at a turn boundary, or
// launches a fresh session with the text as its first prompt.
func (s *Server) deliverChat(r *http.Request, launchID, worker string, req ChatSendRequest, principal string) (resp ChatSendResponse, status int, code, msg string) {
	row, err := s.newestLaunchSession(launchID)
	if err != nil {
		return resp, http.StatusInternalServerError, CodeInternalError, "session lookup failed"
	}
	if row != nil && row.State == string(agentsessions.StateRunning) {
		if h, ok := s.Service.RuntimeHealth(row.ID); ok {
			if h.Health.State != agentsessions.LiveStateIdle {
				return resp, http.StatusConflict, CodeAgentWorking, "agent is mid-turn"
			}
			if err := s.Service.SendTurn(r.Context(), row.ID, req.Text); err != nil {
				switch {
				case errors.Is(err, agentsessions.ErrTurnInFlight):
					return resp, http.StatusConflict, CodeAgentWorking, "agent is mid-turn"
				case errors.Is(err, agentsessions.ErrSessionNotRunning):
					// Raced the session ending: fall through to a fresh launch.
				default:
					return resp, http.StatusBadGateway, CodeTurnFailed, "turn not delivered: " + err.Error()
				}
			} else {
				return ChatSendResponse{LaunchID: launchID, SessionID: row.ID, Mode: "turn"}, http.StatusOK, "", ""
			}
		}
	}

	if worker == "" {
		return resp, http.StatusConflict, CodeWorkerNotConfigured, "no session is running and launch " + launchID + " has no worker; set `worker: <label>` on the launch"
	}
	res, err := s.Service.CreateSessionWithInput(CreateSessionInput{
		Worker:               worker,
		LaunchID:             launchID,
		PromptAppend:         req.Text,
		IdempotencyKey:       "chat:" + launchID + ":" + req.IdempotencyKey,
		IdempotencyPrincipal: principal,
	})
	if err != nil {
		switch {
		case errors.Is(err, store.ErrIdempotencyConflict):
			return resp, http.StatusConflict, CodeIdempotencyConflict, err.Error()
		case errors.Is(err, launch.ErrLaunchNotFound):
			return resp, http.StatusNotFound, CodeNotFound, "chat agent not found"
		case errors.Is(err, launch.ErrDisabled):
			return resp, http.StatusConflict, CodeAgentDisabled, err.Error()
		case errors.Is(err, workerhub.ErrNotEnrolled), errors.Is(err, workerhub.ErrOffline), errors.Is(err, workerhub.ErrChannelUnavailable):
			return resp, http.StatusBadGateway, CodeWorkerUnavailable, "launch " + launchID + " worker unavailable: " + err.Error()
		}
		return resp, http.StatusInternalServerError, CodeInternalError, "session creation failed"
	}
	if !res.Replayed {
		if _, err := s.Service.LaunchSession(res.SessionID); err != nil {
			// The worker's error text can carry paths and stderr; the full cause
			// stays in the daemon log and the session.launch_failed event.
			return resp, http.StatusInternalServerError, CodeInternalError, "session launch failed on the worker (see the session.launch_failed event for session " + res.SessionID + ")"
		}
	}
	status = http.StatusCreated
	if res.Replayed {
		status = http.StatusOK
	}
	return ChatSendResponse{LaunchID: launchID, SessionID: res.SessionID, Mode: "launch", Replayed: res.Replayed}, status, "", ""
}

// begin records an in-flight key. done reports a completed prior delivery;
// conflict reports the key bound to different content or still in flight.
func (c *chatReplays) begin(key, digest string) (resp ChatSendResponse, done, conflict bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]chatReplay{}
	}
	now := time.Now()
	for k, e := range c.entries {
		if now.Sub(e.at) > chatReplayTTL || (len(c.entries) > chatReplayMax && e.done) {
			delete(c.entries, k)
		}
	}
	if e, ok := c.entries[key]; ok {
		if e.digest != digest || !e.done {
			return resp, false, true
		}
		return e.resp, true, false
	}
	c.entries[key] = chatReplay{digest: digest, at: now}
	return resp, false, false
}

func (c *chatReplays) finish(key string, resp ChatSendResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[key]; ok {
		e.done, e.resp = true, resp
		c.entries[key] = e
	}
}

func (c *chatReplays) abandon(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}

// chatEnabled reports whether neither the launch nor its agent is disabled.
func chatEnabled(cat *config.Catalog, l config.Launch) bool {
	if !l.IsEnabled() {
		return false
	}
	a, ok := cat.Agents[l.Agent]
	return !ok || a.IsEnabled()
}
