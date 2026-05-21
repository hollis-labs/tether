package client

// groups_client.go — typed Go client for the daemon's /groups + /mentions
// v060-05 group-messaging endpoints (T-v060-05-06). Mirrors the HTTP
// surface defined in internal/api/groups.go.
//
// Design note. The directory-side endpoints live on *RegistryClient
// because /registry/* are the registry CRUD surface. Group messaging is
// a separate concern (membership state, per-member cursors, mention
// fan-out) with its own auth/identity surrogate model — caller URN is
// supplied per-request, not derived from the registry profile being
// updated. We surface a sibling *GroupsClient so the wire shape and
// method-set stay focused; RegistryClient does not grow groups
// methods. Both share the underlying *Client (and therefore the same
// daemon listen-addr + http.Client).
//
// Error mapping (mirrors registry_client.go):
//
//	HTTP 404 → wraps registry.ErrNotFound
//	HTTP 400 → wraps registry.ErrInvalidRequest (or *ErrAmbiguousMention)
//	HTTP 403 → wraps registry.ErrForbidden
//	HTTP 423 → wraps registry.ErrGroupArchived
//	other 4xx/5xx → wrapped error surfacing the body
//
// Connection-level failures wrap ErrDaemonUnreachable.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// GroupsClient is the typed handle for the daemon's /groups + /mentions
// tree. Get one via Client.Groups(); methods accept ergonomic Go shapes
// and translate to URL-escaped paths at the wire boundary.
type GroupsClient struct {
	c *Client
}

// Groups returns a GroupsClient bound to this Client. The accessor is
// cheap — no allocation beyond the small struct.
func (c *Client) Groups() *GroupsClient {
	return &GroupsClient{c: c}
}

// CreateGroupRequest is the wire shape for Create. CreatorURN goes into
// the Profile.LastUpdatedBy field (auth surrogate per v060-05 D-spec;
// replaced by token auth in v060-03).
type CreateGroupRequest struct {
	DisplayName  string
	Description  string
	Role         string   // group "category" — free-form
	Capabilities []string // topic tags
	CreatorURN   string   // becomes Profile.LastUpdatedBy server-side
}

// Create POSTs /groups with the supplied profile fields. Returns the
// canonical Profile (kind=group, URN minted server-side).
func (gc *GroupsClient) Create(ctx context.Context, req CreateGroupRequest) (registry.Profile, error) {
	p := registry.Profile{
		DisplayName:   req.DisplayName,
		Description:   req.Description,
		Role:          req.Role,
		Capabilities:  req.Capabilities,
		LastUpdatedBy: req.CreatorURN,
	}
	body, err := json.Marshal(p)
	if err != nil {
		return registry.Profile{}, fmt.Errorf("marshal create-group: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, gc.c.baseURL+"/groups", bytes.NewReader(body))
	if err != nil {
		return registry.Profile{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := gc.c.http.Do(httpReq)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return registry.Profile{}, readGroupError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode create-group: %w", err)
	}
	return out, nil
}

// Lookup GETs /groups/{urn}. The kind-enforcement check returns 404
// when the URN is not a group, which surfaces as a wrapped
// registry.ErrNotFound.
func (gc *GroupsClient) Lookup(ctx context.Context, urn string) (registry.Profile, error) {
	path := "/groups/" + url.PathEscape(urn)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readGroupError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode lookup-group: %w", err)
	}
	return out, nil
}

// ListForMember GETs /groups?member=<urn> — the groups memberURN belongs
// to. Returns a non-nil zero-length slice on empty.
func (gc *GroupsClient) ListForMember(ctx context.Context, memberURN string) ([]registry.Profile, error) {
	path := "/groups?member=" + url.QueryEscape(memberURN)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gc.c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readGroupError(resp)
	}
	var env struct {
		Groups []registry.Profile `json:"groups"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode list-for-member: %w", err)
	}
	if env.Groups == nil {
		env.Groups = []registry.Profile{}
	}
	return env.Groups, nil
}

// Archive DELETEs /groups/{urn} with the caller URN as the `as` query
// param. Returns the archived Profile.
func (gc *GroupsClient) Archive(ctx context.Context, grpURN, byURN string) (registry.Profile, error) {
	path := "/groups/" + url.PathEscape(grpURN) + "?as=" + url.QueryEscape(byURN)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, gc.c.baseURL+path, nil)
	if err != nil {
		return registry.Profile{}, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return registry.Profile{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return registry.Profile{}, readGroupError(resp)
	}
	var out registry.Profile
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return registry.Profile{}, fmt.Errorf("decode archive: %w", err)
	}
	return out, nil
}

// ─── membership ──────────────────────────────────────────────────────────

// AddMember POSTs /groups/{urn}/members. Role defaults to "member"
// server-side when empty.
func (gc *GroupsClient) AddMember(ctx context.Context, grpURN, memberURN, byURN string, role registry.MemberRole) (registry.GroupMember, error) {
	body := map[string]string{
		"member": memberURN,
		"by":     byURN,
		"role":   string(role),
	}
	out, err := gc.postJSON(ctx, "/groups/"+url.PathEscape(grpURN)+"/members", body)
	if err != nil {
		return registry.GroupMember{}, err
	}
	var m registry.GroupMember
	if err := json.Unmarshal(out, &m); err != nil {
		return registry.GroupMember{}, fmt.Errorf("decode add-member: %w", err)
	}
	return m, nil
}

// RemoveMember DELETEs /groups/{urn}/members/{member_urn}. byURN
// arrives via the ?as= query.
func (gc *GroupsClient) RemoveMember(ctx context.Context, grpURN, memberURN, byURN string) error {
	path := "/groups/" + url.PathEscape(grpURN) + "/members/" + url.PathEscape(memberURN) +
		"?as=" + url.QueryEscape(byURN)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, gc.c.baseURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return readGroupError(resp)
	}
	return nil
}

// Leave POSTs /groups/{urn}/leave with `{"member": "<urn>"}`.
func (gc *GroupsClient) Leave(ctx context.Context, grpURN, memberURN string) error {
	body := map[string]string{"member": memberURN}
	_, err := gc.postJSON(ctx, "/groups/"+url.PathEscape(grpURN)+"/leave", body)
	return err
}

// SetMemberRole PATCHes /groups/{urn}/members/{member_urn}.
func (gc *GroupsClient) SetMemberRole(ctx context.Context, grpURN, memberURN string, role registry.MemberRole, byURN string) error {
	body := map[string]string{
		"role": string(role),
		"by":   byURN,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal set-role: %w", err)
	}
	path := "/groups/" + url.PathEscape(grpURN) + "/members/" + url.PathEscape(memberURN)
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, gc.c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return readGroupError(resp)
	}
	return nil
}

// ListMembers GETs /groups/{urn}/members.
func (gc *GroupsClient) ListMembers(ctx context.Context, grpURN string) ([]registry.GroupMember, error) {
	path := "/groups/" + url.PathEscape(grpURN) + "/members"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gc.c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readGroupError(resp)
	}
	var env struct {
		Members []registry.GroupMember `json:"members"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode list-members: %w", err)
	}
	if env.Members == nil {
		env.Members = []registry.GroupMember{}
	}
	return env.Members, nil
}

// ─── messaging ───────────────────────────────────────────────────────────

// SendGroupRequest is the wire shape for Send.
type SendGroupRequest struct {
	From        string
	Kind        string // defaults to "message" server-side when empty
	ThreadID    string
	ContentType string
	Payload     json.RawMessage
}

// SendGroupResult is the server's `{message_id, group_seq}` ack.
type SendGroupResult struct {
	MessageID string `json:"message_id"`
	GroupSeq  int64  `json:"group_seq"`
}

// Send POSTs /groups/{urn}/messages. Returns `{message_id, group_seq}`.
// An *registry.ErrAmbiguousMention from the daemon surfaces as a
// wrapped error — callers can errors.As to recover the candidate list.
func (gc *GroupsClient) Send(ctx context.Context, grpURN string, req SendGroupRequest) (SendGroupResult, error) {
	body := map[string]any{
		"from":         req.From,
		"kind":         req.Kind,
		"thread_id":    req.ThreadID,
		"content_type": req.ContentType,
		"payload":      req.Payload,
	}
	b, err := json.Marshal(body)
	if err != nil {
		return SendGroupResult{}, fmt.Errorf("marshal send: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		gc.c.baseURL+"/groups/"+url.PathEscape(grpURN)+"/messages", bytes.NewReader(b))
	if err != nil {
		return SendGroupResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := gc.c.http.Do(httpReq)
	if err != nil {
		return SendGroupResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusCreated {
		return SendGroupResult{}, readGroupError(resp)
	}
	var out SendGroupResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return SendGroupResult{}, fmt.Errorf("decode send: %w", err)
	}
	return out, nil
}

// ListGroupMessagesResult is the paged result of ListMessages.
type ListGroupMessagesResult struct {
	Messages []registry.GroupMessage `json:"messages"`
	NextSeq  int64                   `json:"next_seq"`
}

// ListMessagesParams collects optional filters for ListMessages. as is
// required (the caller's member URN — auth surrogate for v060-05).
type ListMessagesParams struct {
	As       string
	SinceSeq int64
	ThreadID string
	Limit    int
}

// ListMessages GETs /groups/{urn}/messages with the filter params.
// Non-destructive — does NOT bump the read cursor (use MarkRead).
func (gc *GroupsClient) ListMessages(ctx context.Context, grpURN string, p ListMessagesParams) (ListGroupMessagesResult, error) {
	q := url.Values{}
	q.Set("as", p.As)
	if p.SinceSeq != 0 {
		q.Set("since_seq", strconv.FormatInt(p.SinceSeq, 10))
	}
	if p.ThreadID != "" {
		q.Set("thread_id", p.ThreadID)
	}
	if p.Limit != 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	path := "/groups/" + url.PathEscape(grpURN) + "/messages?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gc.c.baseURL+path, nil)
	if err != nil {
		return ListGroupMessagesResult{}, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return ListGroupMessagesResult{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return ListGroupMessagesResult{}, readGroupError(resp)
	}
	var out ListGroupMessagesResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return ListGroupMessagesResult{}, fmt.Errorf("decode list-messages: %w", err)
	}
	if out.Messages == nil {
		out.Messages = []registry.GroupMessage{}
	}
	return out, nil
}

// MarkRead POSTs /groups/{urn}/read. Idempotent / monotonic — smaller
// upToSeq is a no-op server-side.
func (gc *GroupsClient) MarkRead(ctx context.Context, grpURN, memberURN string, upToSeq int64) error {
	body := map[string]any{
		"up_to_seq": upToSeq,
		"as":        memberURN,
	}
	_, err := gc.postJSON(ctx, "/groups/"+url.PathEscape(grpURN)+"/read", body)
	return err
}

// MentionsParams collects optional filters for Mentions.
type MentionsParams struct {
	As    string // required — the member URN whose feed we're reading
	Since time.Time
	Limit int
}

// Mentions GETs /mentions?as=<urn>&since=<ts>&limit=N. Returns the
// notice envelopes from the member's personal inbox that carry a
// `group` key (set by the T-05 daemon-side parser).
func (gc *GroupsClient) Mentions(ctx context.Context, p MentionsParams) ([]registry.GroupMessage, error) {
	q := url.Values{}
	q.Set("as", p.As)
	if !p.Since.IsZero() {
		q.Set("since", p.Since.UTC().Format(time.RFC3339))
	}
	if p.Limit != 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, gc.c.baseURL+"/mentions?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK {
		return nil, readGroupError(resp)
	}
	var env struct {
		Mentions []registry.GroupMessage `json:"mentions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return nil, fmt.Errorf("decode mentions: %w", err)
	}
	if env.Mentions == nil {
		env.Mentions = []registry.GroupMessage{}
	}
	return env.Mentions, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────

// postJSON is the small POST-with-JSON helper used by the
// fixed-shape endpoints (members, leave, read). Returns the raw body
// on 200/201; surfaces the typed daemon error otherwise.
func (gc *GroupsClient) postJSON(ctx context.Context, path string, body any) ([]byte, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal post: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, gc.c.baseURL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := gc.c.http.Do(req)
	if err != nil {
		return nil, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	switch resp.StatusCode {
	case http.StatusOK, http.StatusCreated:
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read post-response: %w", err)
		}
		return raw, nil
	default:
		return nil, readGroupError(resp)
	}
}

// readGroupError reads a daemon error envelope and maps it to a typed
// registry error where possible. Falls back to a generic wrapped error
// for unknown status codes.
func readGroupError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	// Try the ambiguous-mention envelope first (400 with token +
	// candidates). The standard error envelope is a subset so this
	// unmarshal also captures error.code/error.message.
	var ambEnv struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Token      string   `json:"token"`
		Candidates []string `json:"candidates"`
	}
	_ = json.Unmarshal(body, &ambEnv)

	// Map 400 + token+candidates → *registry.ErrAmbiguousMention so
	// callers can errors.As to recover the candidate list. Production
	// callers can then prompt the user to disambiguate.
	if resp.StatusCode == http.StatusBadRequest && len(ambEnv.Candidates) > 0 {
		return &registry.ErrAmbiguousMention{
			Token:      ambEnv.Token,
			Candidates: ambEnv.Candidates,
		}
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, ambEnv.Error.Code, ambEnv.Error.Message, registry.ErrNotFound)
	case http.StatusBadRequest:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, ambEnv.Error.Code, ambEnv.Error.Message, registry.ErrInvalidRequest)
	case http.StatusForbidden:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, ambEnv.Error.Code, ambEnv.Error.Message, registry.ErrForbidden)
	case http.StatusLocked:
		return fmt.Errorf("daemon %d (%s): %s: %w", resp.StatusCode, ambEnv.Error.Code, ambEnv.Error.Message, registry.ErrGroupArchived)
	}
	if ambEnv.Error.Message != "" {
		return fmt.Errorf("daemon %d (%s): %s", resp.StatusCode, ambEnv.Error.Code, ambEnv.Error.Message)
	}
	return fmt.Errorf("daemon %d: %s", resp.StatusCode, string(body))
}
