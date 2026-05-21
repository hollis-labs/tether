// Package api — groups.go is the HTTP surface for group-mailbox
// messaging (v060-05 T-06). The /groups/* tree mounts read+write
// endpoints for the new `group` registry kind: identity (Register/
// Lookup/ListForMember/Archive), membership (Add/Remove/List/SetRole),
// and messaging (Send/List/MarkRead). A sibling /mentions endpoint
// surfaces the per-member mention feed produced by the T-05 parser.
//
// Routing strategy. Mirrors registry.go's prefix-then-parse idiom (no
// Go-1.22 enhanced patterns) so the rest of the api package stays
// consistent. Every URN segment is URL-PathEscape'd by callers; the
// handler url.PathUnescapes after parsing — see registry.go's design
// note for why r.URL.EscapedPath() is non-optional when URNs contain
// embedded slashes.
//
// Error mapping (closest existing error codes; status-code surface
// extends ADR-0010's envelope with 403 + 423 — both expressed via the
// generic invalid_request / not_found / internal_error code vocabulary
// rather than coining new codes, matching the sprint's "no new envelope
// codes" guidance):
//
//	registry.ErrInvalidRequest       → 400 invalid_request
//	*registry.ErrAmbiguousMention    → 400 invalid_request + candidates[]
//	registry.ErrForbidden            → 403 forbidden
//	registry.ErrNotFound             → 404 not_found
//	registry.ErrGroupArchived        → 423 locked (group is read-only)
//	(anything else)                  → 500 internal_error
//
// Caller-identity surrogate. v060-05 has no token auth (lands in
// v060-03). Caller URN is supplied per-request as `last_updated_by`
// on register/create or as `as=<urn>` on list-mentions / mark-read /
// list-messages. Add-member, set-role, send-message etc. pull the
// caller URN from a body field (`by`/`from`) so it survives in the
// audit trail.

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// HTTP code extensions. ADR-0010's envelope keeps the error.code
// vocabulary small; the two new codes here ride on existing 4xx
// semantics so external callers don't grow a new lookup table.
const (
	CodeForbidden = "forbidden"
	CodeLocked    = "locked"
)

// GroupsService is the narrow seam the /groups + /mentions handlers
// depend on. *registry.Service satisfies it through method promotion;
// tests may supply a stub. The interface lists every method called
// from the HTTP layer so it stays auditable as the sprint surface
// grows.
type GroupsService interface {
	// Identity.
	Register(ctx context.Context, kind registry.Kind, p registry.Profile) (registry.Profile, error)
	Lookup(ctx context.Context, urn string) (registry.Profile, error)
	ListGroupsForMember(ctx context.Context, memberURN string) ([]registry.Profile, error)
	ArchiveGroup(ctx context.Context, grpURN, byURN string) (registry.Profile, error)

	// Membership.
	AddMember(ctx context.Context, grpURN, memberURN, byURN string, role registry.MemberRole) (registry.GroupMember, error)
	RemoveMember(ctx context.Context, grpURN, memberURN, byURN string) error
	LeaveGroup(ctx context.Context, grpURN, memberURN string) error
	SetMemberRole(ctx context.Context, grpURN, memberURN string, role registry.MemberRole, byURN string) error
	ListMembers(ctx context.Context, grpURN string) ([]registry.GroupMember, error)

	// Messaging.
	SendToGroup(ctx context.Context, grpURN, fromURN, kind, threadID, contentType string, payload json.RawMessage) (registry.GroupMessage, error)
	ListGroupMessages(ctx context.Context, grpURN, memberURN string, sinceSeq int64, threadID string, limit int) ([]registry.GroupMessage, error)
	MarkRead(ctx context.Context, grpURN, memberURN string, upToSeq int64) error
	GetMyMentions(ctx context.Context, memberURN string, sinceTS time.Time, limit int) ([]registry.GroupMessage, error)
}

// registerGroupRoutes mounts the /groups/ tree + /mentions onto mux. The
// routes are only attached when Server.Groups is non-nil; absent the
// dep, the daemon falls through to its 404 default (matches the
// Catalog/Broker/Registry convention).
func (s *Server) registerGroupRoutes(mux *http.ServeMux) {
	if s.Groups == nil {
		return
	}
	mux.HandleFunc("/groups", s.handleGroupsRoot)
	mux.HandleFunc("/groups/", s.handleGroupsTree)
	mux.HandleFunc("/mentions", s.handleMentions)
}

// handleGroupsRoot services /groups (no trailing slash): POST creates a
// group, GET ?member=<urn> lists groups a member belongs to.
func (s *Server) handleGroupsRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		s.handleGroupCreate(w, r)
	case http.MethodGet:
		s.handleGroupListForMember(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method not allowed on /groups")
	}
}

// handleGroupsTree is the dispatcher for /groups/{urn} and its
// sub-paths. Path shapes:
//
//	/groups/{urn}                              → item (GET lookup, DELETE archive)
//	/groups/{urn}/members                      → collection (POST add, GET list)
//	/groups/{urn}/members/{member_urn}         → item (DELETE remove, PATCH role)
//	/groups/{urn}/leave                        → action (POST leave)
//	/groups/{urn}/messages                     → collection (POST send, GET list)
//	/groups/{urn}/read                         → action (POST mark-read)
//
// {urn} and {member_urn} arrive URL-PathEscape'd and are unescaped here.
func (s *Server) handleGroupsTree(w http.ResponseWriter, r *http.Request) {
	escaped := r.URL.EscapedPath()
	rest := strings.TrimPrefix(escaped, "/groups/")
	if rest == "" {
		writeError(w, http.StatusNotFound, CodeNotFound, "group urn required")
		return
	}
	// At most four segments: urn, action-or-subcoll, member_urn, (unused).
	parts := strings.SplitN(rest, "/", 4)

	urn, err := url.PathUnescape(parts[0])
	if err != nil || urn == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, "malformed group urn in path")
		return
	}

	switch len(parts) {
	case 1:
		// /groups/{urn} — item routes.
		switch r.Method {
		case http.MethodGet:
			s.handleGroupLookup(w, r, urn)
		case http.MethodDelete:
			s.handleGroupArchive(w, r, urn)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method not allowed on group item")
		}
	case 2:
		// /groups/{urn}/{action-or-subcoll}.
		switch parts[1] {
		case "members":
			switch r.Method {
			case http.MethodPost:
				s.handleGroupAddMember(w, r, urn)
			case http.MethodGet:
				s.handleGroupListMembers(w, r, urn)
			default:
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /groups/.../members")
			}
		case "leave":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /groups/.../leave")
				return
			}
			s.handleGroupLeave(w, r, urn)
		case "messages":
			switch r.Method {
			case http.MethodPost:
				s.handleGroupSend(w, r, urn)
			case http.MethodGet:
				s.handleGroupListMessages(w, r, urn)
			default:
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /groups/.../messages")
			}
		case "read":
			if r.Method != http.MethodPost {
				writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
					"method not allowed on /groups/.../read")
				return
			}
			s.handleGroupMarkRead(w, r, urn)
		default:
			writeError(w, http.StatusNotFound, CodeNotFound,
				"unknown group action "+parts[1])
		}
	case 3:
		// /groups/{urn}/members/{member_urn}.
		if parts[1] != "members" {
			writeError(w, http.StatusNotFound, CodeNotFound,
				"unknown group sub-path "+parts[1])
			return
		}
		member, err := url.PathUnescape(parts[2])
		if err != nil || member == "" {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest, "malformed member urn in path")
			return
		}
		switch r.Method {
		case http.MethodDelete:
			s.handleGroupRemoveMember(w, r, urn, member)
		case http.MethodPatch:
			s.handleGroupSetMemberRole(w, r, urn, member)
		default:
			writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
				"method not allowed on group member item")
		}
	default:
		writeError(w, http.StatusNotFound, CodeNotFound, "unknown group sub-path")
	}
}

// ─── identity handlers ──────────────────────────────────────────────────────

// handleGroupCreate services POST /groups. Body is a Profile JSON whose
// LastUpdatedBy field carries the creator URN (v1 surrogate for token
// auth — replaced by v060-03). Returns 201 + canonical Profile.
func (s *Server) handleGroupCreate(w http.ResponseWriter, r *http.Request) {
	var p registry.Profile
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	// Strip caller-supplied identity / timestamp fields so storage
	// defaults apply. Service.Register rejects a non-empty URN with
	// ErrInvalidRequest (the contract teaches callers that the server
	// mints URNs).
	p.Kind = ""
	p.CreatedAt = noTime()
	p.UpdatedAt = noTime()
	p.MuxInstanceID = ""

	out, err := s.Groups.Register(r.Context(), registry.KindGroup, p)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleGroupLookup services GET /groups/{urn}. Returns the Profile
// even if soft-deleted (archived) — callers may need to inspect a
// deprecated row.
func (s *Server) handleGroupLookup(w http.ResponseWriter, r *http.Request, urn string) {
	out, err := s.Groups.Lookup(r.Context(), urn)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	// The route says "kind=group enforcement" — a non-group URN that
	// happens to exist is rejected with 404 so callers can't blindly
	// shadow agent URNs under /groups/.
	if out.Kind != registry.KindGroup {
		writeError(w, http.StatusNotFound, CodeNotFound,
			fmt.Sprintf("urn %q is not a group (kind=%s)", urn, out.Kind))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// handleGroupListForMember services GET /groups?member=<urn>. The
// `member` query param is required; absent it the response is 400 to
// keep the contract explicit (`GET /groups` is NOT a full-tenant
// directory dump — search lives at /registry/groups).
func (s *Server) handleGroupListForMember(w http.ResponseWriter, r *http.Request) {
	member := r.URL.Query().Get("member")
	if member == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"GET /groups requires ?member=<urn>; use /registry/groups for a directory search")
		return
	}
	out, err := s.Groups.ListGroupsForMember(r.Context(), member)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if out == nil {
		out = []registry.Profile{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"groups": out})
}

// handleGroupArchive services DELETE /groups/{urn}. Body (optional)
// may carry `{"by": "<caller URN>"}`. byURN may also arrive via the
// `as` query param for parity with /messages and /read. Returns the
// archived Profile.
func (s *Server) handleGroupArchive(w http.ResponseWriter, r *http.Request, urn string) {
	by := callerFromBodyOrQuery(r, "by")
	if by == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"archive: 'by' (caller URN) is required in body or ?as= query")
		return
	}
	out, err := s.Groups.ArchiveGroup(r.Context(), urn, by)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// ─── membership handlers ───────────────────────────────────────────────────

// addMemberRequest is the body shape for POST /groups/{urn}/members.
type addMemberRequest struct {
	Member string `json:"member"`
	By     string `json:"by"`
	Role   string `json:"role,omitempty"` // defaults to "member"
}

// handleGroupAddMember services POST /groups/{urn}/members.
func (s *Server) handleGroupAddMember(w http.ResponseWriter, r *http.Request, grpURN string) {
	var body addMemberRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	if body.Member == "" || body.By == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"add member: 'member' and 'by' are required in body")
		return
	}
	role := registry.MemberRole(body.Role)
	out, err := s.Groups.AddMember(r.Context(), grpURN, body.Member, body.By, role)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// handleGroupRemoveMember services DELETE /groups/{urn}/members/{member_urn}.
// `by` arrives via query (`?as=<urn>`) or body — body wins when present.
func (s *Server) handleGroupRemoveMember(w http.ResponseWriter, r *http.Request, grpURN, memberURN string) {
	by := callerFromBodyOrQuery(r, "by")
	if by == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"remove member: 'by' (caller URN) is required in body or ?as= query")
		return
	}
	if err := s.Groups.RemoveMember(r.Context(), grpURN, memberURN, by); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGroupLeave services POST /groups/{urn}/leave. Body:
// `{"member": "<urn>"}` — the leaver. v1 has no third-party leave;
// member must equal the caller (semantic check is service-side).
type leaveRequest struct {
	Member string `json:"member"`
}

func (s *Server) handleGroupLeave(w http.ResponseWriter, r *http.Request, grpURN string) {
	var body leaveRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	if body.Member == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"leave: 'member' is required in body")
		return
	}
	if err := s.Groups.LeaveGroup(r.Context(), grpURN, body.Member); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// setMemberRoleRequest is the body shape for PATCH
// /groups/{urn}/members/{member_urn}.
type setMemberRoleRequest struct {
	Role string `json:"role"`
	By   string `json:"by"`
}

// handleGroupSetMemberRole services PATCH /groups/{urn}/members/{member_urn}.
func (s *Server) handleGroupSetMemberRole(w http.ResponseWriter, r *http.Request, grpURN, memberURN string) {
	var body setMemberRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	if body.Role == "" || body.By == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"set role: 'role' and 'by' are required in body")
		return
	}
	if err := s.Groups.SetMemberRole(r.Context(), grpURN, memberURN, registry.MemberRole(body.Role), body.By); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGroupListMembers services GET /groups/{urn}/members.
func (s *Server) handleGroupListMembers(w http.ResponseWriter, r *http.Request, grpURN string) {
	out, err := s.Groups.ListMembers(r.Context(), grpURN)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if out == nil {
		out = []registry.GroupMember{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"members": out})
}

// ─── messaging handlers ────────────────────────────────────────────────────

// sendGroupRequest is the body for POST /groups/{urn}/messages.
type sendGroupRequest struct {
	From        string          `json:"from"`
	Kind        string          `json:"kind,omitempty"` // defaults to "message"
	ThreadID    string          `json:"thread_id,omitempty"`
	ContentType string          `json:"content_type,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

// sendGroupResponse mirrors the sprint's `{message_id, group_seq}`
// success shape — keeps the wire small for high-frequency sends.
type sendGroupResponse struct {
	MessageID string `json:"message_id"`
	GroupSeq  int64  `json:"group_seq"`
}

// handleGroupSend services POST /groups/{urn}/messages. Returns
// 201 + `{message_id, group_seq}`. On *registry.ErrAmbiguousMention,
// returns 400 with a `{candidates: [...]}` array attached to the error
// body so callers can re-issue with a full URN.
func (s *Server) handleGroupSend(w http.ResponseWriter, r *http.Request, grpURN string) {
	var body sendGroupRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	if body.From == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"send: 'from' is required in body")
		return
	}
	kind := body.Kind
	if kind == "" {
		kind = "message"
	}
	gm, err := s.Groups.SendToGroup(r.Context(), grpURN, body.From, kind, body.ThreadID, body.ContentType, body.Payload)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, sendGroupResponse{
		MessageID: gm.ID,
		GroupSeq:  gm.GroupSeq,
	})
}

// listGroupMessagesResponse keeps the `next_seq` cursor visible at
// the envelope level so callers can paginate without re-scanning the
// messages array.
type listGroupMessagesResponse struct {
	Messages []registry.GroupMessage `json:"messages"`
	NextSeq  int64                   `json:"next_seq"`
}

// handleGroupListMessages services GET /groups/{urn}/messages with the
// standard since_seq / thread_id / limit / as query params. `as` is
// the requesting-member URN (caller-supplied auth surrogate per the
// sprint; replaced by token auth in v060-03).
func (s *Server) handleGroupListMessages(w http.ResponseWriter, r *http.Request, grpURN string) {
	q := r.URL.Query()
	as := q.Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"list messages: ?as=<member-urn> is required (caller identity surrogate; v060-03 replaces with token auth)")
		return
	}
	sinceSeq, err := parseInt64Default(q.Get("since_seq"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid since_seq: "+err.Error())
		return
	}
	limit, err := parseIntDefault(q.Get("limit"), 100)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid limit: "+err.Error())
		return
	}
	threadID := q.Get("thread_id")
	out, err := s.Groups.ListGroupMessages(r.Context(), grpURN, as, sinceSeq, threadID, limit)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if out == nil {
		out = []registry.GroupMessage{}
	}
	// Compute next_seq as the highest group_seq in the result set,
	// or echo the caller's sinceSeq when the page is empty so the
	// caller's next request is still monotonic.
	next := sinceSeq
	for _, m := range out {
		if m.GroupSeq > next {
			next = m.GroupSeq
		}
	}
	writeJSON(w, http.StatusOK, listGroupMessagesResponse{
		Messages: out,
		NextSeq:  next,
	})
}

// markReadRequest is the body for POST /groups/{urn}/read.
type markReadRequest struct {
	UpToSeq int64  `json:"up_to_seq"`
	As      string `json:"as"`
}

// handleGroupMarkRead services POST /groups/{urn}/read. Idempotent /
// monotonic: smaller up_to_seq does not lower the cursor (enforced
// service-side).
func (s *Server) handleGroupMarkRead(w http.ResponseWriter, r *http.Request, grpURN string) {
	var body markReadRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid request body: "+err.Error())
		return
	}
	// `as` may also arrive on the query string for parity with /messages
	// listing. Body wins; fall back to query when body is empty.
	caller := body.As
	if caller == "" {
		caller = r.URL.Query().Get("as")
	}
	if caller == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"mark read: 'as' (member URN) is required in body or ?as= query")
		return
	}
	if err := s.Groups.MarkRead(r.Context(), grpURN, caller, body.UpToSeq); err != nil {
		writeGroupError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ─── /mentions ─────────────────────────────────────────────────────────────

// handleMentions services GET /mentions?as=<urn>&since=<RFC3339>&limit=N.
// Surfaces the mention notice envelopes addressed to `as` from the
// member's personal inbox (notices whose payload carries a `group` key).
func (s *Server) handleMentions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, CodeMethodNotAllowed,
			"method not allowed on /mentions")
		return
	}
	q := r.URL.Query()
	as := q.Get("as")
	if as == "" {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"mentions: ?as=<member-urn> is required")
		return
	}
	var since time.Time
	if raw := q.Get("since"); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, CodeInvalidRequest,
				"invalid since (want RFC3339): "+err.Error())
			return
		}
		since = t
	}
	limit, err := parseIntDefault(q.Get("limit"), 50)
	if err != nil {
		writeError(w, http.StatusBadRequest, CodeInvalidRequest,
			"invalid limit: "+err.Error())
		return
	}
	out, err := s.Groups.GetMyMentions(r.Context(), as, since, limit)
	if err != nil {
		writeGroupError(w, err)
		return
	}
	if out == nil {
		out = []registry.GroupMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"mentions": out})
}

// ─── helpers ───────────────────────────────────────────────────────────────

// callerFromBodyOrQuery reads the caller URN from a small request body
// (`{"by": "<urn>"}` or `{"as": "<urn>"}`) or from the ?as= query.
// Returns "" if neither is present. The body is consumed eagerly via
// JSON decoding; failed decodes are treated as "no body" so the caller
// can still pass identity via query (the failure surfaces as 400
// downstream when neither yields a URN).
func callerFromBodyOrQuery(r *http.Request, bodyKey string) string {
	if q := r.URL.Query().Get("as"); q != "" {
		return q
	}
	if r.Body == nil {
		return ""
	}
	defer r.Body.Close() //nolint:errcheck
	var raw map[string]any
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		return ""
	}
	if v, ok := raw[bodyKey].(string); ok && v != "" {
		return v
	}
	if v, ok := raw["as"].(string); ok && v != "" {
		return v
	}
	return ""
}

// parseInt64Default parses raw as int64, returning def when raw is
// empty. Returns an error on malformed input so callers can surface
// the bad query param explicitly.
func parseInt64Default(raw string, def int64) (int64, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// parseIntDefault parses raw as int, returning def when raw is empty.
func parseIntDefault(raw string, def int) (int, error) {
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// ambiguousMentionEnvelope is the wire shape for a 400 response when
// the mention parser surfaces an *ErrAmbiguousMention. Extends the
// standard error envelope with a typed `candidates` slice so callers
// can re-issue with the full URN.
type ambiguousMentionEnvelope struct {
	Error      ErrorDetail `json:"error"`
	Token      string      `json:"token"`
	Candidates []string    `json:"candidates"`
}

// writeGroupError maps the registry-layer error vocabulary to HTTP
// status + envelope. *ErrAmbiguousMention is detected first so the
// caller gets the structured candidates list; all other errors fall
// through to the standard envelope.
func writeGroupError(w http.ResponseWriter, err error) {
	var amb *registry.ErrAmbiguousMention
	if errors.As(err, &amb) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(ambiguousMentionEnvelope{
			Error: ErrorDetail{
				Code:    CodeInvalidRequest,
				Message: amb.Error(),
			},
			Token:      amb.Token,
			Candidates: append([]string{}, amb.Candidates...),
		})
		return
	}
	switch {
	case errors.Is(err, registry.ErrNotFound):
		writeError(w, http.StatusNotFound, CodeNotFound, err.Error())
	case errors.Is(err, registry.ErrInvalidRequest):
		writeError(w, http.StatusBadRequest, CodeInvalidRequest, err.Error())
	case errors.Is(err, registry.ErrForbidden):
		writeError(w, http.StatusForbidden, CodeForbidden, err.Error())
	case errors.Is(err, registry.ErrGroupArchived):
		writeError(w, http.StatusLocked, CodeLocked, err.Error())
	case errors.Is(err, registry.ErrMintExhausted):
		writeError(w, http.StatusServiceUnavailable, CodeInternalError, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, CodeInternalError, err.Error())
	}
}
