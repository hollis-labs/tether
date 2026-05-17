// Command tether_sysop serves the TetherSysop Sysop UI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"sort"
	"time"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/apps/sysop/internal/webui"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// errStateDBUnset signals that the catalog does not configure a state DB
// path. Handlers treat it as "no data yet" rather than a hard error.
var errStateDBUnset = errors.New("state db not configured")

type appServer struct {
	catalogRoot string
}

type healthResponse struct {
	Status      string `json:"status"`
	CatalogRoot string `json:"catalog_root"`
	Error       string `json:"error,omitempty"`
}

type catalogResponse struct {
	Projects  []projectDTO  `json:"projects"`
	Agents    []agentDTO    `json:"agents"`
	Providers []providerDTO `json:"providers"`
	Launches  []launchDTO   `json:"launches"`
}

type projectDTO struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RepoRoot string `json:"repo_root"`
	Mode     string `json:"mode"`
}

type agentDTO struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Roles  []string `json:"roles,omitempty"`
	Skills []string `json:"skills,omitempty"`
}

type providerDTO struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Provider    string `json:"provider"`
	RuntimeKind string `json:"runtime_kind"`
	Command     string `json:"command"`
}

type launchDTO struct {
	ID            string `json:"id"`
	Project       string `json:"project"`
	Agent         string `json:"agent"`
	Provider      string `json:"provider"`
	WorkspaceMode string `json:"workspace_mode"`
	NativeFiles   int    `json:"native_files"`
	BootOverlay   int    `json:"boot_overlay"`
}

type sessionsResponse struct {
	Sessions []sessionDTO `json:"sessions"`
	Error    string       `json:"error,omitempty"`
}

type sessionDTO struct {
	ID             string `json:"id"`
	LaunchID       string `json:"launch_id"`
	ProjectID      string `json:"project_id"`
	LogicalAgentID string `json:"logical_agent_id"`
	ProviderID     string `json:"provider_id"`
	ProviderKind   string `json:"provider_kind"`
	Workspace      string `json:"workspace"`
	State          string `json:"state"`
	PID            *int   `json:"pid,omitempty"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	CreatedAt      string `json:"created_at"`
	UpdatedAt      string `json:"updated_at"`
	EndedAt        string `json:"ended_at,omitempty"`
}

func main() {
	addr := flag.String("addr", ":8947", "HTTP listen address")
	catalogRoot := flag.String("catalog", "~/.tether/catalog", "Tether catalog root")
	flag.Parse()

	server := &appServer{catalogRoot: config.Expand(*catalogRoot)}
	mux := http.NewServeMux()

	mux.HandleFunc("/api/health", server.handleHealth)
	mux.HandleFunc("/api/overview", server.handleOverview)
	mux.HandleFunc("/api/catalog", server.handleCatalog)
	mux.HandleFunc("/api/sessions", server.handleSessions)
	mux.HandleFunc("/api/sessions/detail", server.handleSessionDetail)
	mux.HandleFunc("/api/messages", server.handleMessages)
	mux.HandleFunc("/api/messages/archive", server.handleMessageArchive)
	mux.HandleFunc("/api/messages/read", server.handleMessageMarkRead)
	mux.HandleFunc("/api/activity/events", server.handleActivityEvents)
	mux.HandleFunc("/api/activity/tool-calls", server.handleActivityToolCalls)

	// The Agent Ops UI — served from the embedded frontend build by go-webui.
	webui.Mount(mux)

	log.Printf("TetherSysop listening on http://localhost%s%s/", *addr, webui.BasePath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (s *appServer) handleHealth(w http.ResponseWriter, _ *http.Request) {
	resp := healthResponse{Status: "ok", CatalogRoot: s.catalogRoot}
	if _, err := os.Stat(s.catalogRoot); err != nil {
		resp.Status = "degraded"
		resp.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleCatalog(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	resp := catalogResponse{
		Projects:  make([]projectDTO, 0, len(cat.Projects)),
		Agents:    make([]agentDTO, 0, len(cat.Agents)),
		Providers: make([]providerDTO, 0, len(cat.Providers)),
		Launches:  make([]launchDTO, 0, len(cat.Launches)),
	}
	for _, p := range cat.Projects {
		resp.Projects = append(resp.Projects, projectDTO{
			ID:       p.ID,
			Name:     p.Name,
			RepoRoot: config.Expand(p.RepoRoot),
			Mode:     p.Workspace.DefaultMode,
		})
	}
	for _, a := range cat.Agents {
		resp.Agents = append(resp.Agents, agentDTO{
			ID:     a.ID,
			Name:   a.Name,
			Roles:  a.Roles,
			Skills: a.Skills,
		})
	}
	for _, p := range cat.Providers {
		resp.Providers = append(resp.Providers, providerDTO{
			ID:          p.ID,
			Type:        p.Type,
			Provider:    p.Provider,
			RuntimeKind: p.RuntimeKind,
			Command:     p.Command,
		})
	}
	for _, l := range cat.Launches {
		resp.Launches = append(resp.Launches, launchDTO{
			ID:            l.ID,
			Project:       l.Project,
			Agent:         l.Agent,
			Provider:      l.Provider,
			WorkspaceMode: l.Workspace.Mode,
			NativeFiles:   len(l.Injection.NativeFiles),
			BootOverlay:   len(l.Injection.BootDirOverlay),
		})
	}

	sort.Slice(resp.Projects, func(i, j int) bool { return resp.Projects[i].ID < resp.Projects[j].ID })
	sort.Slice(resp.Agents, func(i, j int) bool { return resp.Agents[i].ID < resp.Agents[j].ID })
	sort.Slice(resp.Providers, func(i, j int) bool { return resp.Providers[i].ID < resp.Providers[j].ID })
	sort.Slice(resp.Launches, func(i, j int) bool { return resp.Launches[i].ID < resp.Launches[j].ID })

	writeJSON(w, http.StatusOK, resp)
}

func (s *appServer) handleSessions(w http.ResponseWriter, _ *http.Request) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		writeJSON(w, http.StatusOK, sessionsResponse{Sessions: []sessionDTO{}})
		return
	}
	db, err := store.Open(dbPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.ListSessions(store.ListSessionsOptions{Limit: 50})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
	out := make([]sessionDTO, 0, len(rows))
	for _, row := range rows {
		out = append(out, sessionDTO{
			ID:             row.ID,
			LaunchID:       row.LaunchID,
			ProjectID:      row.ProjectID,
			LogicalAgentID: row.LogicalAgentID,
			ProviderID:     row.ProviderID,
			ProviderKind:   row.ProviderKind,
			Workspace:      row.Workspace,
			State:          row.State,
			PID:            nullableInt(row.PID.Valid, int(row.PID.Int64)),
			ExitCode:       nullableInt(row.ExitCode.Valid, int(row.ExitCode.Int64)),
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
			EndedAt:        nullableString(row.EndedAt.Valid, row.EndedAt.String),
		})
	}
	writeJSON(w, http.StatusOK, sessionsResponse{Sessions: out})
}

// ─── Messages ────────────────────────────────────────────────────────────────

type messagesResponse struct {
	Messages []messageDTO `json:"messages"`
	Error    string       `json:"error,omitempty"`
}

type messageDTO struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Channel   string `json:"channel,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	ThreadID  string `json:"thread_id,omitempty"`
	InReplyTo string `json:"in_reply_to,omitempty"`
	// Subject and Body are the projected display fields; Payload is the raw
	// (possibly structured-JSON) message payload, kept for detail views.
	Subject     string `json:"subject,omitempty"`
	Body        string `json:"body"`
	Payload     string `json:"payload,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	// Scope is derived from the recipient URN kind: "user", "agent", or "other".
	Scope       string `json:"scope"`
	CreatedAt   string `json:"created_at"`
	DeliveredAt string `json:"delivered_at,omitempty"`
	ConsumedAt  string `json:"consumed_at,omitempty"`
	CanceledAt  string `json:"canceled_at,omitempty"`
	ReadAt      string `json:"read_at,omitempty"`
	ArchivedAt  string `json:"archived_at,omitempty"`
}

type replyRequest struct {
	From        string `json:"from"`
	To          string `json:"to"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	InReplyTo   string `json:"in_reply_to"`
	ThreadID    string `json:"thread_id"`
	ContentType string `json:"content_type"`
}

// handleMessages serves GET (non-destructive list) and POST (send/reply).
func (s *appServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMessagesList(w, r)
	case http.MethodPost:
		s.handleMessageReply(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, messagesResponse{Error: "method not allowed"})
	}
}

func (s *appServer) handleMessagesList(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, messagesResponse{Messages: []messageDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.ListMessages(500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	out := make([]messageDTO, 0, len(rows))
	for _, m := range rows {
		out = append(out, messageDTO{
			ID:          m.ID,
			Kind:        m.Kind,
			Channel:     m.Channel,
			From:        m.FromURN,
			To:          m.ToURN,
			ThreadID:    m.ThreadID,
			InReplyTo:   m.InReplyTo,
			Subject:     m.Subject,
			Body:        m.Body,
			Payload:     m.Payload,
			ContentType: m.ContentType,
			Scope:       scopeOf(m.ToURN),
			CreatedAt:   m.CreatedAt,
			DeliveredAt: m.DeliveredAt,
			ConsumedAt:  m.ConsumedAt,
			CanceledAt:  m.CanceledAt,
			ReadAt:      m.ReadAt,
			ArchivedAt:  m.ArchivedAt,
		})
	}
	writeJSON(w, http.StatusOK, messagesResponse{Messages: out})
}

// handleMessageReply sends a new envelope. Reply is non-destructive — it
// POSTs through the messaging store's Send path (cf. CW-20260517-0003,
// which adds the read/delete semantics this endpoint deliberately omits).
func (s *appServer) handleMessageReply(w http.ResponseWriter, r *http.Request) {
	var req replyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid body: " + err.Error()})
		return
	}
	from, err := messaging.ParseURN(req.From)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid from urn: " + req.From})
		return
	}
	to, err := messaging.ParseURN(req.To)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, messagesResponse{Error: "invalid to urn: " + req.To})
		return
	}
	kind := messaging.Kind(req.Kind)
	if kind == "" {
		kind = messaging.MsgKindResponse
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, messagesResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	env := messaging.Envelope{
		Kind:        kind,
		From:        from,
		To:          to,
		InReplyTo:   req.InReplyTo,
		ThreadID:    req.ThreadID,
		ContentType: req.ContentType,
	}
	if req.Body != "" {
		body, _ := json.Marshal(req.Body)
		env.Payload = body
		if env.ContentType == "" {
			env.ContentType = "text/plain"
		}
	}
	sent, err := db.MessagingStore().Send(r.Context(), env)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, messageDTO{
		ID:          sent.ID,
		Kind:        string(sent.Kind),
		Channel:     string(sent.Channel),
		From:        sent.From.URN(),
		To:          sent.To.URN(),
		ThreadID:    sent.ThreadID,
		InReplyTo:   sent.InReplyTo,
		Body:        payloadBody(string(sent.Payload)),
		ContentType: sent.ContentType,
		Scope:       scopeOf(sent.To.URN()),
		CreatedAt:   sent.CreatedAt.Format(time.RFC3339Nano),
	})
}

// recipientActionRequest is the POST body for the recipient-scoped,
// idempotent message state transitions (archive / mark-read).
type recipientActionRequest struct {
	ID string `json:"id"`
	As string `json:"as"` // recipient URN
}

// handleMessageArchive soft-deletes (archives) a message for its recipient.
func (s *appServer) handleMessageArchive(w http.ResponseWriter, r *http.Request) {
	s.messageRecipientAction(w, r, store.InboxStore.Archive)
}

// handleMessageMarkRead marks a message read by its recipient (idempotent).
func (s *appServer) handleMessageMarkRead(w http.ResponseWriter, r *http.Request) {
	s.messageRecipientAction(w, r, store.InboxStore.MarkRead)
}

// messageRecipientAction runs an idempotent (id, recipient)-scoped message
// transition — Archive or MarkRead — from a POST {id, as} body, mapping the
// store's not-found / wrong-recipient errors to 404 / 409.
func (s *appServer) messageRecipientAction(w http.ResponseWriter, r *http.Request,
	action func(store.InboxStore, context.Context, string, messaging.Address) error) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	var req recipientActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body: " + err.Error()})
		return
	}
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "id required"})
		return
	}
	recipient, err := messaging.ParseURN(req.As)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid recipient urn: " + req.As})
		return
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer db.Close()

	if err := action(db.MessagingStore(), r.Context(), req.ID, recipient); err != nil {
		switch {
		case errors.Is(err, messaging.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "message not found"})
		case errors.Is(err, store.ErrWrongRecipient):
			writeJSON(w, http.StatusConflict, map[string]string{"error": "not the intended recipient"})
		default:
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// ─── Activity ────────────────────────────────────────────────────────────────

type eventsResponse struct {
	Events []eventDTO `json:"events"`
	Error  string     `json:"error,omitempty"`
}

type eventDTO struct {
	Seq       int64  `json:"seq"`
	At        string `json:"at"`
	Scope     string `json:"scope"`
	SessionID string `json:"session_id,omitempty"`
	Kind      string `json:"kind"`
	Payload   string `json:"payload,omitempty"`
}

type toolCallsResponse struct {
	ToolCalls []toolCallDTO `json:"tool_calls"`
	Error     string        `json:"error,omitempty"`
}

type toolCallDTO struct {
	ID         int64  `json:"id"`
	SessionID  string `json:"session_id,omitempty"`
	Server     string `json:"server,omitempty"`
	ToolName   string `json:"tool_name"`
	DurationMs int64  `json:"duration_ms"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Timestamp  string `json:"timestamp"`
}

func (s *appServer) handleActivityEvents(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, eventsResponse{Events: []eventDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.ListRecentEvents(500)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
	out := make([]eventDTO, 0, len(rows))
	for _, e := range rows {
		out = append(out, eventDTO{
			Seq:       e.Seq,
			At:        e.At.Format(time.RFC3339Nano),
			Scope:     e.Scope,
			SessionID: e.SessionID,
			Kind:      e.Kind,
			Payload:   e.PayloadJSON,
		})
	}
	writeJSON(w, http.StatusOK, eventsResponse{Events: out})
}

func (s *appServer) handleActivityToolCalls(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, toolCallsResponse{ToolCalls: []toolCallDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	rows, err := db.QueryProxyEvents(store.ProxyEventFilter{Limit: 500})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	// QueryProxyEvents returns oldest-first; reverse for newest-first.
	out := make([]toolCallDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		ev := rows[i]
		out = append(out, toolCallDTO{
			ID:         ev.ID,
			SessionID:  ev.SessionID,
			Server:     ev.Server,
			ToolName:   ev.ToolName,
			DurationMs: ev.DurationMs,
			OK:         ev.OK,
			Error:      ev.Error,
			Timestamp:  ev.Timestamp.Format(time.RFC3339Nano),
		})
	}
	writeJSON(w, http.StatusOK, toolCallsResponse{ToolCalls: out})
}

// ─── Overview ────────────────────────────────────────────────────────────────

type nameCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type overviewResponse struct {
	Sessions  overviewSessions  `json:"sessions"`
	ToolCalls overviewToolCalls `json:"tool_calls"`
	Messages  overviewMessages  `json:"messages"`
	Events    overviewEvents    `json:"events"`
	Catalog   overviewCatalog   `json:"catalog"`
	Health    healthResponse    `json:"health"`
	Error     string            `json:"error,omitempty"`
}

type overviewSessions struct {
	Total      int   `json:"total"`
	Running    int   `json:"running"`
	Ended      int   `json:"ended"`
	SuccessPct int   `json:"success_pct"`
	AvgSeconds int   `json:"avg_seconds"`
	Trend      []int `json:"trend"`
}

type overviewToolCalls struct {
	Total      int         `json:"total"`
	OK         int         `json:"ok"`
	Errors     int         `json:"errors"`
	SuccessPct int         `json:"success_pct"`
	P50ms      int64       `json:"p50_ms"`
	P95ms      int64       `json:"p95_ms"`
	TopTools   []nameCount `json:"top_tools"`
	TopErrors  []nameCount `json:"top_errors"`
	Trend      []int       `json:"trend"`
}

type overviewMessages struct {
	Total    int         `json:"total"`
	Unread   int         `json:"unread"`
	Archived int         `json:"archived"`
	ByKind   []nameCount `json:"by_kind"`
	Trend    []int       `json:"trend"`
}

type overviewEvents struct {
	Total   int         `json:"total"`
	ByScope []nameCount `json:"by_scope"`
	Trend   []int       `json:"trend"`
}

type overviewCatalog struct {
	Projects  int `json:"projects"`
	Agents    int `json:"agents"`
	Providers int `json:"providers"`
	Launches  int `json:"launches"`
}

const overviewTrendBuckets = 24

// handleOverview aggregates recent DB activity into a single dashboard
// payload. Catalog/health come from the catalog; the rest from the state
// DB. A missing state DB yields zeroed stats rather than an error.
func (s *appServer) handleOverview(w http.ResponseWriter, _ *http.Request) {
	resp := overviewResponse{Health: healthResponse{Status: "ok", CatalogRoot: s.catalogRoot}}
	if _, err := os.Stat(s.catalogRoot); err != nil {
		resp.Health.Status = "degraded"
		resp.Health.Error = err.Error()
	}
	if cat, err := config.Load(s.catalogRoot); err == nil {
		resp.Catalog = overviewCatalog{
			Projects:  len(cat.Projects),
			Agents:    len(cat.Agents),
			Providers: len(cat.Providers),
			Launches:  len(cat.Launches),
		}
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, resp)
		return
	}
	if err != nil {
		resp.Error = err.Error()
		writeJSON(w, http.StatusOK, resp)
		return
	}
	defer db.Close()

	if sessions, serr := db.ListSessions(store.ListSessionsOptions{Limit: 1000}); serr == nil {
		var starts []time.Time
		var durations []float64
		for _, row := range sessions {
			if t, ok := parseTime(row.CreatedAt); ok {
				starts = append(starts, t)
			}
			if row.State == "running" {
				resp.Sessions.Running++
			}
			if row.EndedAt.Valid && row.EndedAt.String != "" {
				resp.Sessions.Ended++
				if row.ExitCode.Valid && row.ExitCode.Int64 == 0 {
					resp.Sessions.SuccessPct++ // tallied, converted to pct below
				}
				if c, ok1 := parseTime(row.CreatedAt); ok1 {
					if e, ok2 := parseTime(row.EndedAt.String); ok2 {
						durations = append(durations, e.Sub(c).Seconds())
					}
				}
			}
		}
		resp.Sessions.Total = len(sessions)
		if resp.Sessions.Ended > 0 {
			resp.Sessions.SuccessPct = resp.Sessions.SuccessPct * 100 / resp.Sessions.Ended
		}
		if len(durations) > 0 {
			var sum float64
			for _, d := range durations {
				sum += d
			}
			resp.Sessions.AvgSeconds = int(sum / float64(len(durations)))
		}
		resp.Sessions.Trend = bucketCounts(starts, overviewTrendBuckets)
	}

	if proxy, perr := db.QueryProxyEvents(store.ProxyEventFilter{Limit: 500}); perr == nil {
		toolCounts := map[string]int{}
		errCounts := map[string]int{}
		var durs []int64
		var times []time.Time
		for _, ev := range proxy {
			toolCounts[ev.ToolName]++
			durs = append(durs, ev.DurationMs)
			times = append(times, ev.Timestamp)
			if ev.OK {
				resp.ToolCalls.OK++
			} else {
				resp.ToolCalls.Errors++
				errCounts[ev.ToolName]++
			}
		}
		resp.ToolCalls.Total = len(proxy)
		if resp.ToolCalls.Total > 0 {
			resp.ToolCalls.SuccessPct = resp.ToolCalls.OK * 100 / resp.ToolCalls.Total
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		resp.ToolCalls.P50ms = pctile(durs, 0.50)
		resp.ToolCalls.P95ms = pctile(durs, 0.95)
		resp.ToolCalls.TopTools = topN(toolCounts, 5)
		resp.ToolCalls.TopErrors = topN(errCounts, 5)
		resp.ToolCalls.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	if msgs, merr := db.ListMessages(1000); merr == nil {
		kindCounts := map[string]int{}
		var times []time.Time
		for _, m := range msgs {
			kindCounts[m.Kind]++
			if m.ReadAt == "" && m.CanceledAt == "" {
				resp.Messages.Unread++
			}
			if m.ArchivedAt != "" {
				resp.Messages.Archived++
			}
			if t, ok := parseTime(m.CreatedAt); ok {
				times = append(times, t)
			}
		}
		resp.Messages.Total = len(msgs)
		resp.Messages.ByKind = topN(kindCounts, 6)
		resp.Messages.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	if evs, eerr := db.ListRecentEvents(1000); eerr == nil {
		scopeCounts := map[string]int{}
		var times []time.Time
		for _, e := range evs {
			scopeCounts[e.Scope]++
			times = append(times, e.At)
		}
		resp.Events.Total = len(evs)
		resp.Events.ByScope = topN(scopeCounts, 6)
		resp.Events.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── Session detail ──────────────────────────────────────────────────────────

type attachmentDTO struct {
	ID         string `json:"id"`
	ClientKind string `json:"client_kind"`
	AttachedAt string `json:"attached_at"`
	DetachedAt string `json:"detached_at,omitempty"`
}

type checkpointDTO struct {
	ID                 string `json:"id"`
	Status             string `json:"status,omitempty"`
	TaskID             string `json:"task_id,omitempty"`
	WorkflowID         string `json:"workflow_id,omitempty"`
	Summary            string `json:"summary,omitempty"`
	CompletedWork      string `json:"completed_work,omitempty"`
	PendingWork        string `json:"pending_work,omitempty"`
	KeyDecisions       string `json:"key_decisions,omitempty"`
	NextRecommendation string `json:"next_recommendation,omitempty"`
	CreatedAt          string `json:"created_at"`
	SourceSessionID    string `json:"source_session_id,omitempty"`
}

type sessionDetailResponse struct {
	Session     sessionDTO      `json:"session"`
	GroupID     string          `json:"group_id,omitempty"`
	Events      []eventDTO      `json:"events"`
	Attachments []attachmentDTO `json:"attachments"`
	LaunchPlan  string          `json:"launch_plan,omitempty"`
	Checkpoints []checkpointDTO `json:"checkpoints"`
	Error       string          `json:"error,omitempty"`
}

// handleSessionDetail returns a composite view of one session: its row,
// per-session events, client attachments, launch plan, and the checkpoints
// of its logical agent.
func (s *appServer) handleSessionDetail(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		writeJSON(w, http.StatusBadRequest, sessionDetailResponse{Error: "id query param required"})
		return
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, sessionDetailResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionDetailResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	row, err := db.GetSession(id)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionDetailResponse{Error: err.Error()})
		return
	}
	if row == nil {
		writeJSON(w, http.StatusNotFound, sessionDetailResponse{Error: "session not found"})
		return
	}

	resp := sessionDetailResponse{
		Session: sessionDTO{
			ID:             row.ID,
			LaunchID:       row.LaunchID,
			ProjectID:      row.ProjectID,
			LogicalAgentID: row.LogicalAgentID,
			ProviderID:     row.ProviderID,
			ProviderKind:   row.ProviderKind,
			Workspace:      row.Workspace,
			State:          row.State,
			PID:            nullableInt(row.PID.Valid, int(row.PID.Int64)),
			ExitCode:       nullableInt(row.ExitCode.Valid, int(row.ExitCode.Int64)),
			CreatedAt:      row.CreatedAt,
			UpdatedAt:      row.UpdatedAt,
			EndedAt:        nullableString(row.EndedAt.Valid, row.EndedAt.String),
		},
		Events:      []eventDTO{},
		Attachments: []attachmentDTO{},
		Checkpoints: []checkpointDTO{},
	}
	if row.SessionGroupID.Valid {
		resp.GroupID = row.SessionGroupID.String
	}

	if evs, eerr := db.ListEventsBySession(id, 200, 0); eerr == nil {
		for _, e := range evs {
			resp.Events = append(resp.Events, eventDTO{
				Seq:       e.Seq,
				At:        e.At.Format(time.RFC3339Nano),
				Scope:     e.Scope,
				SessionID: e.SessionID,
				Kind:      e.Kind,
				Payload:   e.PayloadJSON,
			})
		}
	}

	if atts, aerr := db.ListClientAttachments(id); aerr == nil {
		for _, a := range atts {
			resp.Attachments = append(resp.Attachments, attachmentDTO{
				ID:         a.ID,
				ClientKind: a.ClientKind,
				AttachedAt: a.AttachedAt,
				DetachedAt: nullableString(a.DetachedAt.Valid, a.DetachedAt.String),
			})
		}
	}

	if plan, perr := db.GetLaunchPlan(id); perr == nil && plan != nil {
		if b, merr := json.MarshalIndent(plan, "", "  "); merr == nil {
			resp.LaunchPlan = string(b)
		}
	}

	if row.LogicalAgentID != "" {
		if cps, cerr := db.ListCheckpointsByLogicalAgent(row.LogicalAgentID); cerr == nil {
			for _, c := range cps {
				resp.Checkpoints = append(resp.Checkpoints, checkpointDTO{
					ID:                 c.ID,
					Status:             c.Status,
					TaskID:             c.TaskID,
					WorkflowID:         c.WorkflowID,
					Summary:            c.Summary,
					CompletedWork:      c.CompletedWork,
					PendingWork:        c.PendingWork,
					KeyDecisions:       c.KeyDecisions,
					NextRecommendation: c.NextRecommendation,
					CreatedAt:          c.CreatedAt,
					SourceSessionID:    c.SourceSessionID,
				})
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// ─── Aggregate helpers ───────────────────────────────────────────────────────

// parseTime parses an RFC3339(Nano) timestamp, tolerating either precision.
func parseTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// pctile returns the p-quantile (0..1) of an already-sorted slice.
func pctile(sorted []int64, p float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(float64(len(sorted)-1) * p)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// topN returns the n highest-count entries, ties broken by name.
func topN(counts map[string]int, n int) []nameCount {
	out := make([]nameCount, 0, len(counts))
	for k, v := range counts {
		out = append(out, nameCount{Name: k, Count: v})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// bucketCounts distributes timestamps into n equal-width buckets spanning
// [min,max], returned oldest→newest — a sparkline-ready series.
func bucketCounts(times []time.Time, n int) []int {
	buckets := make([]int, n)
	if len(times) == 0 || n <= 0 {
		return buckets
	}
	lo, hi := times[0], times[0]
	for _, t := range times {
		if t.Before(lo) {
			lo = t
		}
		if t.After(hi) {
			hi = t
		}
	}
	span := hi.Sub(lo)
	if span <= 0 {
		buckets[n-1] = len(times)
		return buckets
	}
	for _, t := range times {
		idx := int(float64(t.Sub(lo)) / float64(span) * float64(n))
		if idx >= n {
			idx = n - 1
		}
		if idx < 0 {
			idx = 0
		}
		buckets[idx]++
	}
	return buckets
}

// openStateDB loads the catalog and opens the Tether state DB. Returns
// errStateDBUnset when the catalog configures no state DB path.
func (s *appServer) openStateDB() (*store.Store, error) {
	cat, err := config.Load(s.catalogRoot)
	if err != nil {
		return nil, err
	}
	dbPath := config.Expand(cat.Global.Catalog.Defaults.StateDB)
	if dbPath == "" {
		return nil, errStateDBUnset
	}
	return store.Open(dbPath)
}

// scopeOf classifies a recipient URN as "user", "agent", or "other".
func scopeOf(toURN string) string {
	addr, err := messaging.ParseURN(toURN)
	if err != nil {
		return "other"
	}
	switch addr.Kind {
	case messaging.KindUser:
		return "user"
	case messaging.KindAgent:
		return "agent"
	default:
		return "other"
	}
}

// payloadBody unwraps a message payload for display: a JSON-encoded string
// payload is unquoted to its text; anything else is returned verbatim.
func payloadBody(payload string) string {
	if payload == "" {
		return ""
	}
	var s string
	if err := json.Unmarshal([]byte(payload), &s); err == nil {
		return s
	}
	return payload
}

func nullableInt(valid bool, value int) *int {
	if !valid {
		return nil
	}
	return &value
}

func nullableString(valid bool, value string) string {
	if !valid {
		return ""
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
