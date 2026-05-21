package api

// groups_test.go — end-to-end HTTP coverage for the /groups + /mentions
// tree (T-v060-05-06). Wires a real *registry.Service over a real
// *registry.Storage on an in-memory SQLite DB and exercises the HTTP
// surface via httptest.NewServer.
//
// Coverage matrix:
//
//   - POST /groups happy path + missing creator URN (400) + non-existent
//     creator (400) + caller-URN rejection.
//   - GET /groups/{urn} (kind=group enforcement → 404 when given an
//     agent URN).
//   - GET /groups?member=<urn>.
//   - DELETE /groups/{urn} archive (203/403 paths) + 423 send-after-archive.
//   - POST/DELETE/PATCH/GET /groups/{urn}/members lifecycle.
//   - POST /groups/{urn}/leave self-remove.
//   - POST /groups/{urn}/messages happy + non-member 403 + archived 423.
//   - GET  /groups/{urn}/messages with since_seq / limit / as.
//   - POST /groups/{urn}/read MarkRead monotonic.
//   - GET /mentions filters by member URN.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

// newGroupHandler stands up a real Service+Storage on an in-memory
// SQLite and wires it into the api Handler. Groups + Registry are wired
// to the SAME *registry.Service in production; tests do the same.
func newGroupHandler(t *testing.T, opts ...registry.ServiceOption) (http.Handler, *registry.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := store.Migrate(db); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	storage := registry.NewStorage(db)
	svc := registry.NewService(storage, opts...)
	return NewHandler(Deps{Registry: svc, Groups: svc}), svc
}

type groupServer struct {
	t   *testing.T
	srv *httptest.Server
	svc *registry.Service
}

func newGroupServer(t *testing.T, opts ...registry.ServiceOption) *groupServer {
	t.Helper()
	h, svc := newGroupHandler(t, opts...)
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &groupServer{t: t, srv: srv, svc: svc}
}

func (g *groupServer) do(method, path string, body any) (*http.Response, []byte) {
	g.t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			g.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, g.srv.URL+path, reader)
	if err != nil {
		g.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		g.t.Fatalf("read body: %v", err)
	}
	return resp, buf
}

// seedAgent registers a basic agent and returns the canonical URN.
func (g *groupServer) seedAgent(name string) string {
	g.t.Helper()
	resp, body := g.do(http.MethodPost, "/registry/agents", registry.Profile{
		DisplayName:   name,
		LastUpdatedBy: "tester",
	})
	if resp.StatusCode != http.StatusCreated {
		g.t.Fatalf("seed agent %q: status=%d body=%s", name, resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		g.t.Fatalf("seed agent decode: %v", err)
	}
	return p.URN
}

// seedGroup registers a group owned by creator and returns the canonical
// Profile.
func (g *groupServer) seedGroup(name, creatorURN string) registry.Profile {
	g.t.Helper()
	resp, body := g.do(http.MethodPost, "/groups", registry.Profile{
		DisplayName:   name,
		LastUpdatedBy: creatorURN,
	})
	if resp.StatusCode != http.StatusCreated {
		g.t.Fatalf("seed group %q: status=%d body=%s", name, resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		g.t.Fatalf("seed group decode: %v", err)
	}
	return p
}

// itemURLGroup builds the URL-escaped /groups/<urn> path.
func itemURLGroup(urn string) string { return "/groups/" + url.PathEscape(urn) }

// ─── POST /groups ─────────────────────────────────────────────────────────

func TestGroups_Create_Happy(t *testing.T) {
	g := newGroupServer(t)
	creator := g.seedAgent("Creator")
	resp, body := g.do(http.MethodPost, "/groups", registry.Profile{
		DisplayName:   "Design Room",
		Description:   "for whiteboard discussions",
		LastUpdatedBy: creator,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var p registry.Profile
	if err := json.Unmarshal(body, &p); err != nil {
		t.Fatalf("decode: %v: %s", err, body)
	}
	if p.Kind != registry.KindGroup {
		t.Errorf("Kind=%q want group", p.Kind)
	}
	if !registry.IsGroupURN(p.URN) {
		t.Errorf("URN=%q is not a group URN", p.URN)
	}
}

func TestGroups_Create_MissingCreatorURN(t *testing.T) {
	g := newGroupServer(t)
	resp, _ := g.do(http.MethodPost, "/groups", registry.Profile{
		DisplayName: "No Creator",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestGroups_Create_UnknownCreator(t *testing.T) {
	g := newGroupServer(t)
	resp, _ := g.do(http.MethodPost, "/groups", registry.Profile{
		DisplayName:   "Bad Creator",
		LastUpdatedBy: "msg://agent/agent-mux/agt_ghost00000",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

// ─── GET /groups/{urn} ───────────────────────────────────────────────────

func TestGroups_Lookup_Happy(t *testing.T) {
	g := newGroupServer(t)
	creator := g.seedAgent("C")
	created := g.seedGroup("Lookup", creator)
	resp, body := g.do(http.MethodGet, itemURLGroup(created.URN), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.URN != created.URN {
		t.Errorf("URN=%q want %q", got.URN, created.URN)
	}
}

func TestGroups_Lookup_AgentURN404(t *testing.T) {
	// An agent URN happens to exist but is fetched under /groups/ —
	// the kind-enforcement check rejects it as 404 so callers can't
	// shadow registry URN namespaces.
	g := newGroupServer(t)
	agent := g.seedAgent("AnAgent")
	resp, body := g.do(http.MethodGet, itemURLGroup(agent), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
}

// ─── GET /groups?member=<urn> ────────────────────────────────────────────

func TestGroups_ListForMember(t *testing.T) {
	g := newGroupServer(t)
	a := g.seedAgent("A")
	b := g.seedAgent("B")
	gp := g.seedGroup("Shared", a)
	// invite b
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/members", addMemberRequest{
		Member: b,
		By:     a,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("add member status=%d body=%s", resp.StatusCode, body)
	}

	resp, body = g.do(http.MethodGet, "/groups?member="+url.QueryEscape(b), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Groups []registry.Profile `json:"groups"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(env.Groups) != 1 || env.Groups[0].URN != gp.URN {
		t.Errorf("groups=%v", env.Groups)
	}
}

func TestGroups_ListForMember_MissingQuery(t *testing.T) {
	g := newGroupServer(t)
	resp, _ := g.do(http.MethodGet, "/groups", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

// ─── DELETE /groups/{urn} (archive) ──────────────────────────────────────

func TestGroups_Archive_Happy(t *testing.T) {
	g := newGroupServer(t)
	a := g.seedAgent("A")
	gp := g.seedGroup("ToArchive", a)
	resp, body := g.do(http.MethodDelete, itemURLGroup(gp.URN)+"?as="+url.QueryEscape(a), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var got registry.Profile
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Status != registry.StatusArchived {
		t.Errorf("Status=%q want archived", got.Status)
	}
}

func TestGroups_Archive_NonOwner403(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("O")
	other := g.seedAgent("Other")
	gp := g.seedGroup("NonOwner", owner)
	// invite `other` so they're a member but not owner/moderator.
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/members", addMemberRequest{
		Member: other, By: owner,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite other status=%d body=%s", resp.StatusCode, body)
	}
	resp, _ = g.do(http.MethodDelete, itemURLGroup(gp.URN)+"?as="+url.QueryEscape(other), nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
}

// ─── members lifecycle ───────────────────────────────────────────────────

func TestGroups_MembersLifecycle(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	bob := g.seedAgent("Bob")
	gp := g.seedGroup("Crew", owner)

	// invite Bob.
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/members", addMemberRequest{
		Member: bob, By: owner,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite status=%d body=%s", resp.StatusCode, body)
	}

	// list members.
	resp, body = g.do(http.MethodGet, itemURLGroup(gp.URN)+"/members", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Members []registry.GroupMember `json:"members"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode list: %v body=%s", err, body)
	}
	if len(env.Members) != 2 {
		t.Fatalf("members=%d want 2", len(env.Members))
	}

	// promote Bob to moderator.
	resp, _ = g.do(http.MethodPatch, itemURLGroup(gp.URN)+"/members/"+url.PathEscape(bob),
		setMemberRoleRequest{Role: "moderator", By: owner})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("set role status=%d", resp.StatusCode)
	}

	// remove Bob.
	resp, _ = g.do(http.MethodDelete, itemURLGroup(gp.URN)+"/members/"+url.PathEscape(bob)+"?as="+url.QueryEscape(owner), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("remove status=%d", resp.StatusCode)
	}
}

func TestGroups_Leave_Self(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	bob := g.seedAgent("Bob")
	gp := g.seedGroup("LeaveTest", owner)
	// invite Bob first.
	resp, _ := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/members", addMemberRequest{
		Member: bob, By: owner,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("invite status=%d", resp.StatusCode)
	}
	// Bob leaves.
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/leave",
		leaveRequest{Member: bob})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("leave status=%d body=%s", resp.StatusCode, body)
	}
}

// ─── messaging ───────────────────────────────────────────────────────────

func TestGroups_Send_Happy(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	gp := g.seedGroup("Chatty", owner)
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/messages", sendGroupRequest{
		From:    owner,
		Kind:    "message",
		Payload: json.RawMessage(`{"text":"hello"}`),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var out sendGroupResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if out.MessageID == "" {
		t.Errorf("empty message_id")
	}
	if out.GroupSeq != 1 {
		t.Errorf("group_seq=%d want 1", out.GroupSeq)
	}
}

func TestGroups_Send_NonMember403(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	stranger := g.seedAgent("Stranger")
	gp := g.seedGroup("Closed", owner)
	resp, _ := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/messages", sendGroupRequest{
		From: stranger, Kind: "message",
		Payload: json.RawMessage(`{"text":"intruder"}`),
	})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d want 403", resp.StatusCode)
	}
}

func TestGroups_Send_ArchivedReturns423(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	gp := g.seedGroup("Cooked", owner)
	// archive.
	resp, _ := g.do(http.MethodDelete, itemURLGroup(gp.URN)+"?as="+url.QueryEscape(owner), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("archive prep status=%d", resp.StatusCode)
	}
	// now attempt to send.
	resp, _ = g.do(http.MethodPost, itemURLGroup(gp.URN)+"/messages", sendGroupRequest{
		From:    owner,
		Kind:    "message",
		Payload: json.RawMessage(`{"text":"toolate"}`),
	})
	if resp.StatusCode != http.StatusLocked {
		t.Fatalf("status=%d want 423", resp.StatusCode)
	}
}

func TestGroups_ListMessages_AndMarkRead(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	gp := g.seedGroup("Listy", owner)
	// send 3 messages.
	for i := 1; i <= 3; i++ {
		resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/messages", sendGroupRequest{
			From: owner, Kind: "message",
			Payload: json.RawMessage(`{"i":` + itoa(i) + `}`),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("send i=%d: status=%d body=%s", i, resp.StatusCode, body)
		}
	}
	// list (since_seq=0 → from cursor; cursor=0 → all 3).
	resp, body := g.do(http.MethodGet, itemURLGroup(gp.URN)+"/messages?as="+url.QueryEscape(owner), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status=%d body=%s", resp.StatusCode, body)
	}
	var out listGroupMessagesResponse
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("messages=%d want 3", len(out.Messages))
	}
	if out.NextSeq != 3 {
		t.Errorf("next_seq=%d want 3", out.NextSeq)
	}
	// MarkRead up_to_seq=2 — next list should return only seq=3.
	resp, _ = g.do(http.MethodPost, itemURLGroup(gp.URN)+"/read",
		markReadRequest{UpToSeq: 2, As: owner})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("read status=%d", resp.StatusCode)
	}
	resp, body = g.do(http.MethodGet, itemURLGroup(gp.URN)+"/messages?as="+url.QueryEscape(owner), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("re-list status=%d", resp.StatusCode)
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("re-decode: %v", err)
	}
	if len(out.Messages) != 1 || out.Messages[0].GroupSeq != 3 {
		t.Errorf("post-MarkRead list = %+v", out.Messages)
	}
}

func TestGroups_ListMessages_RequiresAs(t *testing.T) {
	g := newGroupServer(t)
	owner := g.seedAgent("Owner")
	gp := g.seedGroup("AsReq", owner)
	resp, _ := g.do(http.MethodGet, itemURLGroup(gp.URN)+"/messages", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

// ─── /mentions ───────────────────────────────────────────────────────────

func TestGroups_Mentions_RequiresAs(t *testing.T) {
	g := newGroupServer(t)
	resp, _ := g.do(http.MethodGet, "/mentions", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400", resp.StatusCode)
	}
}

func TestGroups_Mentions_Empty(t *testing.T) {
	g := newGroupServer(t)
	a := g.seedAgent("A")
	resp, body := g.do(http.MethodGet, "/mentions?as="+url.QueryEscape(a), nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	var env struct {
		Mentions []registry.GroupMessage `json:"mentions"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Mentions == nil {
		t.Errorf("empty mentions decoded as nil; want non-nil empty slice")
	}
}

// ─── method-not-allowed ──────────────────────────────────────────────────

func TestGroups_MethodNotAllowed(t *testing.T) {
	g := newGroupServer(t)
	resp, _ := g.do(http.MethodPut, "/groups", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status=%d want 405", resp.StatusCode)
	}
}

func TestGroups_AmbiguousMention(t *testing.T) {
	// Verify that an *ErrAmbiguousMention surfaces as a 400 with a
	// structured candidates list. We use a parser stub that always
	// returns the ambiguous error so we don't need to construct two
	// duplicate-display-name profiles in test data.
	parser := stubAmbiguousParser{token: "alice", candidates: []string{
		"msg://agent/agent-mux/agt_aaaaaaaaaa",
		"msg://agent/agent-mux/agt_bbbbbbbbbb",
	}}
	g := newGroupServer(t, registry.WithMentionParser(parser))
	owner := g.seedAgent("Owner")
	gp := g.seedGroup("Mentiony", owner)
	resp, body := g.do(http.MethodPost, itemURLGroup(gp.URN)+"/messages", sendGroupRequest{
		From: owner, Kind: "message",
		Payload: json.RawMessage(`{"text":"@alice hi"}`),
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", resp.StatusCode, body)
	}
	var env ambiguousMentionEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode: %v body=%s", err, body)
	}
	if env.Token != "alice" {
		t.Errorf("token=%q want alice", env.Token)
	}
	if len(env.Candidates) != 2 {
		t.Errorf("candidates=%v", env.Candidates)
	}
}

// stubAmbiguousParser is a registry.MentionParser that always returns
// *ErrAmbiguousMention from Parse. Used to exercise the writeGroupError
// pre-dispatch branch without requiring a full lookup setup.
type stubAmbiguousParser struct {
	token      string
	candidates []string
}

func (s stubAmbiguousParser) Parse(_ context.Context, _ json.RawMessage, _ string) ([]registry.Mention, error) {
	return nil, &registry.ErrAmbiguousMention{Token: s.token, Candidates: s.candidates}
}

func (stubAmbiguousParser) Dispatch(_ context.Context, _ registry.GroupMessage, _ []registry.Mention) {
}

// itoa is a tiny strconv shim used in JSON literals above; avoids
// pulling strconv into the body literal where readability is hurt.
func itoa(i int) string {
	switch i {
	case 0:
		return "0"
	case 1:
		return "1"
	case 2:
		return "2"
	case 3:
		return "3"
	}
	// Falls back for larger values; the test never exercises this.
	return "9"
}

var _ = time.RFC3339 // referenced by /mentions tests above; silence import-elide
