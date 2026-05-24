// Command tether_sysop serves the TetherSysop Sysop UI.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/apps/sysop/internal/webui"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	launchplan "github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
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
	Profile       string `json:"profile,omitempty"`
	LaunchPlan    string `json:"launch_plan,omitempty"`
	PlanError     string `json:"plan_error,omitempty"`
}

type sessionsResponse struct {
	Sessions []sessionDTO `json:"sessions"`
	Total    int          `json:"total"`
	Running  int          `json:"running"`
	Ended    int          `json:"ended"`
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
	mux.HandleFunc("/api/messages/groups", server.handleMessageGroups)
	mux.HandleFunc("/api/messages/groups/create", server.handleMessageGroupCreate)
	mux.HandleFunc("/api/messages/agents", server.handleMessageAgents)
	mux.HandleFunc("/api/activity/events", server.handleActivityEvents)
	mux.HandleFunc("/api/activity/tool-calls", server.handleActivityToolCalls)
	mux.HandleFunc("/api/mcp/servers", server.handleMCPServers)
	mux.HandleFunc("/api/mcp/tools", server.handleMCPTools)

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
		profileJSON := prettyJSON(l)
		planJSON := ""
		planError := ""
		if plan, err := launchplan.Resolve(cat, launchplan.Input{
			LaunchID:    l.ID,
			CatalogRoot: s.catalogRoot,
		}); err != nil {
			planError = err.Error()
		} else {
			planJSON = prettyJSON(plan)
		}
		resp.Launches = append(resp.Launches, launchDTO{
			ID:            l.ID,
			Project:       l.Project,
			Agent:         l.Agent,
			Provider:      l.Provider,
			WorkspaceMode: l.Workspace.Mode,
			NativeFiles:   len(l.Injection.NativeFiles),
			BootOverlay:   len(l.Injection.BootDirOverlay),
			Profile:       profileJSON,
			LaunchPlan:    planJSON,
			PlanError:     planError,
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

	totals, err := sessionTotals(db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, sessionsResponse{Error: err.Error()})
		return
	}
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
	writeJSON(w, http.StatusOK, sessionsResponse{
		Sessions: out,
		Total:    totals.Total,
		Running:  totals.Running,
		Ended:    totals.Ended,
	})
}

type sessionStats struct {
	Total   int
	Running int
	Ended   int
}

func sessionTotals(db *store.Store) (sessionStats, error) {
	rows, err := db.DB().Query(`SELECT state, COALESCE(ended_at, '') FROM sessions`)
	if err != nil {
		return sessionStats{}, err
	}
	defer rows.Close()

	var stats sessionStats
	for rows.Next() {
		var state, endedAt string
		if err := rows.Scan(&state, &endedAt); err != nil {
			return sessionStats{}, err
		}
		stats.Total++
		if state == "running" {
			stats.Running++
		}
		if endedAt != "" {
			stats.Ended++
		}
	}
	return stats, rows.Err()
}

// ─── Messages ────────────────────────────────────────────────────────────────

type messagesResponse struct {
	Messages []messageDTO  `json:"messages"`
	Totals   messageTotals `json:"totals"`
	Error    string        `json:"error,omitempty"`
}

type messageTotals struct {
	Total  int               `json:"total"`
	User   messageScopeStats `json:"user"`
	Agent  messageScopeStats `json:"agent"`
	Other  messageScopeStats `json:"other"`
	Groups messageScopeStats `json:"groups"`
}

type messageScopeStats struct {
	Total    int `json:"total"`
	Unread   int `json:"unread"`
	Archived int `json:"archived"`
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

type groupsResponse struct {
	Groups []groupDTO `json:"groups"`
	Error  string     `json:"error,omitempty"`
}

type groupDTO struct {
	URN         string            `json:"urn"`
	DisplayName string            `json:"display_name"`
	Title       string            `json:"title,omitempty"`
	Description string            `json:"description,omitempty"`
	Status      string            `json:"status"`
	CreatedAt   string            `json:"created_at"`
	UpdatedAt   string            `json:"updated_at"`
	Members     []groupMemberDTO  `json:"members"`
	Messages    []groupMessageDTO `json:"messages"`
	updatedAt   time.Time
}

type groupMemberDTO struct {
	MemberURN   string `json:"member_urn"`
	DisplayName string `json:"display_name,omitempty"`
	Role        string `json:"role"`
	JoinedAt    string `json:"joined_at"`
	LastReadSeq int64  `json:"last_read_seq"`
}

type groupMessageDTO struct {
	ID          string `json:"id"`
	GroupURN    string `json:"group_urn"`
	GroupSeq    int64  `json:"group_seq"`
	FromURN     string `json:"from_urn"`
	Kind        string `json:"kind"`
	ThreadID    string `json:"thread_id,omitempty"`
	Subject     string `json:"subject,omitempty"`
	Body        string `json:"body"`
	Payload     string `json:"payload,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	CreatedAt   string `json:"created_at"`
}

type groupReplyRequest struct {
	GroupURN    string `json:"group_urn"`
	From        string `json:"from"`
	Kind        string `json:"kind"`
	Body        string `json:"body"`
	ThreadID    string `json:"thread_id,omitempty"`
	ContentType string `json:"content_type,omitempty"`
}

type groupCreateRequest struct {
	DisplayName string `json:"display_name"`
	Description string `json:"description,omitempty"`
	CreatorURN  string `json:"creator_urn"`
}

type messageAgentsResponse struct {
	Agents []messageAgentDTO `json:"agents"`
	Error  string            `json:"error,omitempty"`
}

type messageAgentDTO struct {
	URN         string `json:"urn"`
	DisplayName string `json:"display_name"`
	Title       string `json:"title,omitempty"`
	Status      string `json:"status"`
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

func (s *appServer) handleMessageGroups(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleMessageGroupsList(w, r)
	case http.MethodPost:
		s.handleMessageGroupReply(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, groupsResponse{Error: "method not allowed"})
	}
}

func (s *appServer) handleMessageGroupCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, groupsResponse{Error: "method not allowed"})
		return
	}
	var req groupCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.DisplayName) == "" || strings.TrimSpace(req.CreatorURN) == "" {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "display_name and creator_urn are required"})
		return
	}

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, groupsResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	svc := registry.NewService(regStore)
	group, err := svc.Register(r.Context(), registry.KindGroup, registry.Profile{
		DisplayName:   strings.TrimSpace(req.DisplayName),
		Description:   strings.TrimSpace(req.Description),
		LastUpdatedBy: strings.TrimSpace(req.CreatorURN),
	})
	if err != nil {
		writeRegistryActionError(w, err)
		return
	}
	members, err := regStore.ListMembers(r.Context(), group.URN)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, groupToDTO(group, members, nil))
}

func (s *appServer) handleMessageAgents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, messageAgentsResponse{Error: "method not allowed"})
		return
	}
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, messageAgentsResponse{Agents: []messageAgentDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageAgentsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	agents, err := regStore.Search(r.Context(), registry.KindAgent, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messageAgentsResponse{Error: err.Error()})
		return
	}
	out := make([]messageAgentDTO, 0, len(agents))
	for _, a := range agents {
		out = append(out, messageAgentDTO{
			URN:         a.URN,
			DisplayName: a.DisplayName,
			Title:       a.Title,
			Status:      string(a.Status),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DisplayName != out[j].DisplayName {
			return out[i].DisplayName < out[j].DisplayName
		}
		return out[i].URN < out[j].URN
	})
	writeJSON(w, http.StatusOK, messageAgentsResponse{Agents: out})
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

	totals, err := countMessages(db)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, messagesResponse{Error: err.Error()})
		return
	}
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
	writeJSON(w, http.StatusOK, messagesResponse{Messages: out, Totals: totals})
}

func countMessages(db *store.Store) (messageTotals, error) {
	rows, err := db.DB().Query(
		`SELECT to_urn, COALESCE(read_at, ''), COALESCE(archived_at, ''),
		        COALESCE(canceled_at, ''), COALESCE(group_urn, '')
		   FROM messages`)
	if err != nil {
		return messageTotals{}, err
	}
	defer rows.Close()

	var totals messageTotals
	for rows.Next() {
		var toURN, readAt, archivedAt, canceledAt, groupURN string
		if err := rows.Scan(&toURN, &readAt, &archivedAt, &canceledAt, &groupURN); err != nil {
			return messageTotals{}, err
		}
		totals.Total++
		var bucket *messageScopeStats
		if groupURN != "" {
			bucket = &totals.Groups
		} else {
			switch scopeOf(toURN) {
			case "user":
				bucket = &totals.User
			case "agent":
				bucket = &totals.Agent
			default:
				bucket = &totals.Other
			}
		}
		bucket.Total++
		if readAt == "" && canceledAt == "" {
			bucket.Unread++
		}
		if archivedAt != "" {
			bucket.Archived++
		}
	}
	return totals, rows.Err()
}

func (s *appServer) handleMessageGroupsList(w http.ResponseWriter, r *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, groupsResponse{Groups: []groupDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	groups, err := regStore.Search(r.Context(), registry.KindGroup, registry.Filter{Status: registry.StatusAny})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}

	out := make([]groupDTO, 0, len(groups))
	for _, g := range groups {
		members, err := regStore.ListMembers(r.Context(), g.URN)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
			return
		}
		msgs, err := regStore.ListGroupMessages(r.Context(), g.URN, -1, "", 200, time.Time{})
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
			return
		}
		out = append(out, groupToDTO(g, members, msgs))
	}

	sort.Slice(out, func(i, j int) bool {
		left := out[i].updatedAt
		right := out[j].updatedAt
		if !left.Equal(right) {
			return left.After(right)
		}
		return out[i].DisplayName < out[j].DisplayName
	})
	writeJSON(w, http.StatusOK, groupsResponse{Groups: out})
}

func (s *appServer) handleMessageGroupReply(w http.ResponseWriter, r *http.Request) {
	var req groupReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "invalid body: " + err.Error()})
		return
	}
	if req.GroupURN == "" || req.From == "" || strings.TrimSpace(req.Body) == "" {
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: "group_urn, from, and body are required"})
		return
	}
	kind := req.Kind
	if kind == "" {
		kind = "message"
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "text/plain"
	}
	payload, _ := json.Marshal(strings.TrimSpace(req.Body))

	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusServiceUnavailable, groupsResponse{Error: "no state db configured"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	regStore := registry.NewStorage(db.DB())
	svc := registry.NewService(regStore)
	sent, err := svc.SendToGroup(r.Context(), req.GroupURN, req.From, kind, req.ThreadID, contentType, payload)
	if err != nil {
		writeRegistryActionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, groupMessageToDTO(sent))
}

func writeRegistryActionError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, registry.ErrInvalidRequest):
		writeJSON(w, http.StatusBadRequest, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrForbidden):
		writeJSON(w, http.StatusForbidden, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrGroupArchived):
		writeJSON(w, http.StatusLocked, groupsResponse{Error: err.Error()})
	case errors.Is(err, registry.ErrNotFound):
		writeJSON(w, http.StatusNotFound, groupsResponse{Error: err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, groupsResponse{Error: err.Error()})
	}
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
	Total  int        `json:"total"`
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
	Total     int           `json:"total"`
	Error     string        `json:"error,omitempty"`
}

type toolCallDTO struct {
	ID           int64  `json:"id"`
	SessionID    string `json:"session_id,omitempty"`
	Server       string `json:"server,omitempty"`
	ToolName     string `json:"tool_name"`
	ArgsSchemaFP string `json:"args_schema_fp,omitempty"`
	DurationMs   int64  `json:"duration_ms"`
	OK           bool   `json:"ok"`
	Error        string `json:"error,omitempty"`
	Timestamp    string `json:"timestamp"`
	Payload      string `json:"payload,omitempty"`
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

	total, err := db.CountEvents()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, eventsResponse{Error: err.Error()})
		return
	}
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
	writeJSON(w, http.StatusOK, eventsResponse{Events: out, Total: total})
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

	total, err := db.CountProxyEvents(store.ProxyEventFilter{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	rows, err := db.QueryProxyEvents(store.ProxyEventFilter{Limit: 500})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, toolCallsResponse{Error: err.Error()})
		return
	}
	// QueryProxyEvents returns oldest-first; reverse for newest-first.
	out := make([]toolCallDTO, 0, len(rows))
	for i := len(rows) - 1; i >= 0; i-- {
		ev := rows[i]
		payload := toolCallPayload(ev)
		out = append(out, toolCallDTO{
			ID:           ev.ID,
			SessionID:    ev.SessionID,
			Server:       ev.Server,
			ToolName:     ev.ToolName,
			ArgsSchemaFP: ev.ArgsSchemaFP,
			DurationMs:   ev.DurationMs,
			OK:           ev.OK,
			Error:        ev.Error,
			Timestamp:    ev.Timestamp.Format(time.RFC3339Nano),
			Payload:      payload,
		})
	}
	writeJSON(w, http.StatusOK, toolCallsResponse{ToolCalls: out, Total: total})
}

func toolCallPayload(ev store.ProxyEvent) string {
	raw, err := json.Marshal(events.ToolCallEvent{
		SessionID:    ev.SessionID,
		ToolName:     ev.ToolName,
		Server:       ev.Server,
		ArgsSchemaFP: ev.ArgsSchemaFP,
		DurationMs:   ev.DurationMs,
		OK:           ev.OK,
		Error:        ev.Error,
		Timestamp:    ev.Timestamp,
	})
	if err != nil {
		return ""
	}
	return string(raw)
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
	Total      int         `json:"total"`
	Running    int         `json:"running"`
	Ended      int         `json:"ended"`
	SuccessPct int         `json:"success_pct"`
	FailurePct int         `json:"failure_pct"`
	AvgSeconds int         `json:"avg_seconds"`
	Recent24h  int         `json:"recent_24h"`
	Trend      []int       `json:"trend"`
	ByState    []nameCount `json:"by_state"`
	ByProvider []nameCount `json:"by_provider"`
	ByProject  []nameCount `json:"by_project"`
}

type overviewToolCalls struct {
	Total      int         `json:"total"`
	OK         int         `json:"ok"`
	Errors     int         `json:"errors"`
	SuccessPct int         `json:"success_pct"`
	P50ms      int64       `json:"p50_ms"`
	P95ms      int64       `json:"p95_ms"`
	AvgMs      int64       `json:"avg_ms"`
	Recent1h   int         `json:"recent_1h"`
	SlowCalls  int         `json:"slow_calls"`
	Sessions   int         `json:"sessions"`
	TopTools   []nameCount `json:"top_tools"`
	TopErrors  []nameCount `json:"top_errors"`
	ByServer   []nameCount `json:"by_server"`
	Latency    []nameCount `json:"latency"`
	Trend      []int       `json:"trend"`
}

type overviewMessages struct {
	Total     int         `json:"total"`
	Unread    int         `json:"unread"`
	Archived  int         `json:"archived"`
	Recent24h int         `json:"recent_24h"`
	ByKind    []nameCount `json:"by_kind"`
	ByScope   []nameCount `json:"by_scope"`
	Trend     []int       `json:"trend"`
}

type overviewEvents struct {
	Total     int         `json:"total"`
	Recent1h  int         `json:"recent_1h"`
	LatestSeq int64       `json:"latest_seq"`
	ByScope   []nameCount `json:"by_scope"`
	ByKind    []nameCount `json:"by_kind"`
	Trend     []int       `json:"trend"`
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

	populateOverviewSessions(db, &resp)

	if total, terr := db.CountProxyEvents(store.ProxyEventFilter{}); terr == nil {
		resp.ToolCalls.Total = total
	}
	if errors, eerr := db.CountProxyEvents(store.ProxyEventFilter{ErrorsOnly: true}); eerr == nil {
		resp.ToolCalls.Errors = errors
		resp.ToolCalls.OK = resp.ToolCalls.Total - errors
		if resp.ToolCalls.Total > 0 {
			resp.ToolCalls.SuccessPct = resp.ToolCalls.OK * 100 / resp.ToolCalls.Total
		}
	}
	if proxy, perr := db.QueryProxyEvents(store.ProxyEventFilter{Limit: -1}); perr == nil {
		toolCounts := map[string]int{}
		errCounts := map[string]int{}
		serverCounts := map[string]int{}
		sessionSet := map[string]struct{}{}
		latencyCounts := map[string]int{}
		var durs []int64
		var times []time.Time
		var sum int64
		cutoff := time.Now().UTC().Add(-time.Hour)
		for _, ev := range proxy {
			toolCounts[ev.ToolName]++
			server := ev.Server
			if server == "" {
				server = "native"
			}
			serverCounts[server]++
			if ev.SessionID != "" {
				sessionSet[ev.SessionID] = struct{}{}
			}
			durs = append(durs, ev.DurationMs)
			sum += ev.DurationMs
			times = append(times, ev.Timestamp)
			if ev.Timestamp.After(cutoff) {
				resp.ToolCalls.Recent1h++
			}
			if ev.DurationMs >= 1000 {
				resp.ToolCalls.SlowCalls++
			}
			latencyCounts[latencyBand(ev.DurationMs)]++
			if !ev.OK {
				errCounts[ev.ToolName]++
			}
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		resp.ToolCalls.P50ms = pctile(durs, 0.50)
		resp.ToolCalls.P95ms = pctile(durs, 0.95)
		if len(durs) > 0 {
			resp.ToolCalls.AvgMs = sum / int64(len(durs))
		}
		resp.ToolCalls.Sessions = len(sessionSet)
		resp.ToolCalls.TopTools = topN(toolCounts, 5)
		resp.ToolCalls.TopErrors = topN(errCounts, 5)
		resp.ToolCalls.ByServer = topN(serverCounts, 6)
		resp.ToolCalls.Latency = orderedLatencyCounts(latencyCounts)
		resp.ToolCalls.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	populateOverviewMessages(db, &resp)

	if total, terr := db.CountEvents(); terr == nil {
		resp.Events.Total = total
	}
	if evs, eerr := db.ListRecentEvents(1000); eerr == nil {
		scopeCounts := map[string]int{}
		kindCounts := map[string]int{}
		var times []time.Time
		cutoff := time.Now().UTC().Add(-time.Hour)
		for _, e := range evs {
			scopeCounts[e.Scope]++
			kindCounts[e.Kind]++
			times = append(times, e.At)
			if e.At.After(cutoff) {
				resp.Events.Recent1h++
			}
			if e.Seq > resp.Events.LatestSeq {
				resp.Events.LatestSeq = e.Seq
			}
		}
		resp.Events.ByScope = topN(scopeCounts, 6)
		resp.Events.ByKind = topN(kindCounts, 8)
		resp.Events.Trend = bucketCounts(times, overviewTrendBuckets)
	}

	writeJSON(w, http.StatusOK, resp)
}

func populateOverviewSessions(db *store.Store, resp *overviewResponse) {
	rows, err := db.DB().Query(
		`SELECT state, provider_id, project_id, created_at,
		        COALESCE(ended_at, ''), exit_code
		   FROM sessions`)
	if err != nil {
		return
	}
	defer rows.Close()

	stateCounts := map[string]int{}
	providerCounts := map[string]int{}
	projectCounts := map[string]int{}
	var starts []time.Time
	var durations []float64
	successes := 0
	failures := 0
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	for rows.Next() {
		var state, providerID, projectID, createdAt, endedAt string
		var exitCode sql.NullInt64
		if err := rows.Scan(&state, &providerID, &projectID, &createdAt, &endedAt, &exitCode); err != nil {
			return
		}
		resp.Sessions.Total++
		stateCounts[state]++
		providerCounts[valueOr(providerID, "unknown")]++
		projectCounts[valueOr(projectID, "unknown")]++
		if state == "running" {
			resp.Sessions.Running++
		}
		if c, ok := parseTime(createdAt); ok {
			starts = append(starts, c)
			if c.After(cutoff) {
				resp.Sessions.Recent24h++
			}
			if endedAt != "" {
				if e, ok := parseTime(endedAt); ok {
					durations = append(durations, e.Sub(c).Seconds())
				}
			}
		}
		if endedAt != "" {
			resp.Sessions.Ended++
			if exitCode.Valid && exitCode.Int64 == 0 {
				successes++
			} else {
				failures++
			}
		}
	}
	if resp.Sessions.Ended > 0 {
		resp.Sessions.SuccessPct = successes * 100 / resp.Sessions.Ended
		resp.Sessions.FailurePct = failures * 100 / resp.Sessions.Ended
	}
	if len(durations) > 0 {
		var sum float64
		for _, d := range durations {
			sum += d
		}
		resp.Sessions.AvgSeconds = int(sum / float64(len(durations)))
	}
	resp.Sessions.ByState = topN(stateCounts, 6)
	resp.Sessions.ByProvider = topN(providerCounts, 6)
	resp.Sessions.ByProject = topN(projectCounts, 6)
	resp.Sessions.Trend = bucketCounts(starts, overviewTrendBuckets)
}

func populateOverviewMessages(db *store.Store, resp *overviewResponse) {
	rows, err := db.DB().Query(
		`SELECT kind, to_urn, created_at, COALESCE(read_at, ''),
		        COALESCE(archived_at, ''), COALESCE(canceled_at, ''),
		        COALESCE(group_urn, '')
		   FROM messages`)
	if err != nil {
		return
	}
	defer rows.Close()

	kindCounts := map[string]int{}
	scopeCounts := map[string]int{}
	var times []time.Time
	cutoff := time.Now().UTC().Add(-24 * time.Hour)

	for rows.Next() {
		var kind, toURN, createdAt, readAt, archivedAt, canceledAt, groupURN string
		if err := rows.Scan(&kind, &toURN, &createdAt, &readAt, &archivedAt, &canceledAt, &groupURN); err != nil {
			return
		}
		resp.Messages.Total++
		kindCounts[kind]++
		scope := scopeOf(toURN)
		if groupURN != "" {
			scope = "group"
		}
		scopeCounts[scope]++
		if readAt == "" && canceledAt == "" {
			resp.Messages.Unread++
		}
		if archivedAt != "" {
			resp.Messages.Archived++
		}
		if t, ok := parseTime(createdAt); ok {
			times = append(times, t)
			if t.After(cutoff) {
				resp.Messages.Recent24h++
			}
		}
	}
	resp.Messages.ByKind = topN(kindCounts, 6)
	resp.Messages.ByScope = topN(scopeCounts, 6)
	resp.Messages.Trend = bucketCounts(times, overviewTrendBuckets)
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

func valueOr(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func latencyBand(ms int64) string {
	switch {
	case ms < 10:
		return "<10ms"
	case ms < 100:
		return "10-99ms"
	case ms < 500:
		return "100-499ms"
	case ms < 1000:
		return "500-999ms"
	default:
		return ">=1s"
	}
}

func orderedLatencyCounts(counts map[string]int) []nameCount {
	order := []string{"<10ms", "10-99ms", "100-499ms", "500-999ms", ">=1s"}
	out := make([]nameCount, 0, len(order))
	for _, name := range order {
		if counts[name] > 0 {
			out = append(out, nameCount{Name: name, Count: counts[name]})
		}
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

// ─── MCP ─────────────────────────────────────────────────────────────────────

type mcpServerDTO struct {
	ID        string   `json:"id"`
	Transport string   `json:"transport"`
	Command   string   `json:"command,omitempty"`
	Args      []string `json:"args,omitempty"`
	URL       string   `json:"url,omitempty"`
	EnvKeys   []string `json:"env_keys,omitempty"`
	HasToken  bool     `json:"has_token"`
	Scopes    []string `json:"scopes,omitempty"`
	Tags      []string `json:"tags,omitempty"`
	Enabled   bool     `json:"enabled"`
}

type mcpServersResponse struct {
	Servers []mcpServerDTO `json:"servers"`
	Error   string         `json:"error,omitempty"`
}

// handleMCPServers lists the upstream MCP servers from the catalog
// (<catalog>/mcp-servers/*.yaml). Token is redacted to a boolean and only
// env keys are returned, since both can carry secrets.
func (s *appServer) handleMCPServers(w http.ResponseWriter, _ *http.Request) {
	entries, err := config.LoadMCPServers(s.catalogRoot)
	if err != nil {
		writeJSON(w, http.StatusOK, mcpServersResponse{Servers: []mcpServerDTO{}, Error: err.Error()})
		return
	}
	out := make([]mcpServerDTO, 0, len(entries))
	for _, e := range entries {
		envKeys := make([]string, 0, len(e.Env))
		for k := range e.Env {
			envKeys = append(envKeys, k)
		}
		sort.Strings(envKeys)
		out = append(out, mcpServerDTO{
			ID:        e.ID,
			Transport: e.Transport,
			Command:   e.Command,
			Args:      e.Args,
			URL:       e.URL,
			EnvKeys:   envKeys,
			HasToken:  e.Token != "",
			Scopes:    e.Scopes,
			Tags:      e.Tags,
			Enabled:   e.Enabled == nil || *e.Enabled,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	writeJSON(w, http.StatusOK, mcpServersResponse{Servers: out})
}

type mcpToolDTO struct {
	Name       string `json:"name"`
	Server     string `json:"server"`
	Calls      int    `json:"calls"`
	Errors     int    `json:"errors"`
	SuccessPct int    `json:"success_pct"`
	AvgMs      int64  `json:"avg_ms"`
	P95ms      int64  `json:"p95_ms"`
	LastSeen   string `json:"last_seen"`
}

type mcpToolsResponse struct {
	Tools      []mcpToolDTO `json:"tools"`
	TotalCalls int          `json:"total_calls"`
	Error      string       `json:"error,omitempty"`
}

// handleMCPTools returns a usage-centric tool list aggregated from the
// proxy_events ring buffer: per tool, its owning server, call volume,
// success rate, and latency. The live registry of *available* tools is
// daemon-only — see Torque CW-20260517-0047.
func (s *appServer) handleMCPTools(w http.ResponseWriter, _ *http.Request) {
	db, err := s.openStateDB()
	if errors.Is(err, errStateDBUnset) {
		writeJSON(w, http.StatusOK, mcpToolsResponse{Tools: []mcpToolDTO{}})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}
	defer db.Close()

	totalCalls, err := db.CountProxyEvents(store.ProxyEventFilter{})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}
	events, err := db.QueryProxyEvents(store.ProxyEventFilter{Limit: -1})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, mcpToolsResponse{Error: err.Error()})
		return
	}

	type agg struct {
		server   string
		calls    int
		errors   int
		durs     []int64
		lastSeen time.Time
	}
	byTool := map[string]*agg{}
	for _, ev := range events {
		a := byTool[ev.ToolName]
		if a == nil {
			a = &agg{}
			byTool[ev.ToolName] = a
		}
		if ev.Server != "" {
			a.server = ev.Server
		}
		a.calls++
		if !ev.OK {
			a.errors++
		}
		a.durs = append(a.durs, ev.DurationMs)
		if ev.Timestamp.After(a.lastSeen) {
			a.lastSeen = ev.Timestamp
		}
	}

	out := make([]mcpToolDTO, 0, len(byTool))
	for name, a := range byTool {
		sort.Slice(a.durs, func(i, j int) bool { return a.durs[i] < a.durs[j] })
		var sum int64
		for _, d := range a.durs {
			sum += d
		}
		var avg int64
		if len(a.durs) > 0 {
			avg = sum / int64(len(a.durs))
		}
		successPct := 0
		if a.calls > 0 {
			successPct = (a.calls - a.errors) * 100 / a.calls
		}
		server := a.server
		if server == "" {
			server = "native"
		}
		out = append(out, mcpToolDTO{
			Name:       name,
			Server:     server,
			Calls:      a.calls,
			Errors:     a.errors,
			SuccessPct: successPct,
			AvgMs:      avg,
			P95ms:      pctile(a.durs, 0.95),
			LastSeen:   a.lastSeen.Format(time.RFC3339Nano),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Calls != out[j].Calls {
			return out[i].Calls > out[j].Calls
		}
		return out[i].Name < out[j].Name
	})
	writeJSON(w, http.StatusOK, mcpToolsResponse{Tools: out, TotalCalls: totalCalls})
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

func groupToDTO(g registry.Profile, members []registry.GroupMember, msgs []registry.GroupMessage) groupDTO {
	memberDTOs := make([]groupMemberDTO, 0, len(members))
	for _, m := range members {
		memberDTOs = append(memberDTOs, groupMemberDTO{
			MemberURN:   m.MemberURN,
			DisplayName: m.DisplayName,
			Role:        string(m.Role),
			JoinedAt:    m.JoinedAt.Format(time.RFC3339Nano),
			LastReadSeq: m.LastReadSeq,
		})
	}
	sort.Slice(memberDTOs, func(i, j int) bool {
		ri := groupMemberRoleRank(memberDTOs[i].Role)
		rj := groupMemberRoleRank(memberDTOs[j].Role)
		if ri != rj {
			return ri < rj
		}
		if memberDTOs[i].DisplayName != memberDTOs[j].DisplayName {
			return memberDTOs[i].DisplayName < memberDTOs[j].DisplayName
		}
		return memberDTOs[i].MemberURN < memberDTOs[j].MemberURN
	})

	messageDTOs := make([]groupMessageDTO, 0, len(msgs))
	for _, msg := range msgs {
		messageDTOs = append(messageDTOs, groupMessageToDTO(msg))
	}

	return groupDTO{
		URN:         g.URN,
		DisplayName: g.DisplayName,
		Title:       g.Title,
		Description: g.Description,
		Status:      string(g.Status),
		CreatedAt:   g.CreatedAt.Format(time.RFC3339Nano),
		UpdatedAt:   g.UpdatedAt.Format(time.RFC3339Nano),
		Members:     memberDTOs,
		Messages:    messageDTOs,
		updatedAt:   g.UpdatedAt,
	}
}

func groupMemberRoleRank(role string) int {
	switch role {
	case "owner":
		return 0
	case "moderator":
		return 1
	case "member":
		return 2
	default:
		return 3
	}
}

func groupMessageToDTO(m registry.GroupMessage) groupMessageDTO {
	subject, body := groupPayloadText(m.Payload)
	return groupMessageDTO{
		ID:          m.ID,
		GroupURN:    m.GroupURN,
		GroupSeq:    m.GroupSeq,
		FromURN:     m.FromURN,
		Kind:        m.Kind,
		ThreadID:    m.ThreadID,
		Subject:     subject,
		Body:        body,
		Payload:     string(m.Payload),
		ContentType: m.ContentType,
		CreatedAt:   m.CreatedAt.Format(time.RFC3339Nano),
	}
}

func groupPayloadText(payload json.RawMessage) (subject, body string) {
	trimmed := strings.TrimSpace(string(payload))
	if trimmed == "" {
		return "", ""
	}
	switch trimmed[0] {
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &obj); err != nil {
			return "", trimmed
		}
		return firstPayloadString(obj, "subject", "title"),
			firstPayloadString(obj, "body", "summary", "text", "message")
	case '"':
		var s string
		if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
			return "", s
		}
		return "", trimmed
	default:
		return "", trimmed
	}
}

func firstPayloadString(obj map[string]json.RawMessage, keys ...string) string {
	for _, k := range keys {
		raw, ok := obj[k]
		if !ok {
			continue
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
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

func prettyJSON(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return ""
	}
	return string(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}
