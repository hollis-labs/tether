package client

// groups_client_test.go — wire-format compliance tests for GroupsClient.
// Same shape as registry_client_test.go: canned httptest.Server
// responses + assertions on the request URL/method + decoded shape.
// Service correctness is covered by internal/api/groups_test.go; these
// tests verify the wire-level behavior in isolation.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/registry"
)

// newGroupsClient stands up a httptest.Server with the given handler
// and returns a Client pointed at it.
func newGroupsClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return &Client{baseURL: srv.URL, http: srv.Client()}
}

// ─── Create ─────────────────────────────────────────────────────────────

func TestGroupsClient_Create_Happy(t *testing.T) {
	want := registry.Profile{
		URN:         "msg://group/agent-mux/grp_aaaaaaaaaa",
		Kind:        registry.KindGroup,
		DisplayName: "Design Room",
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if r.URL.Path != "/groups" {
			t.Errorf("path = %q, want /groups", r.URL.Path)
		}
		var p registry.Profile
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			t.Errorf("decode body: %v", err)
		}
		if p.DisplayName != "Design Room" {
			t.Errorf("DisplayName = %q", p.DisplayName)
		}
		if p.LastUpdatedBy != "msg://agent/agent-mux/agt_creator00" {
			t.Errorf("LastUpdatedBy = %q", p.LastUpdatedBy)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(want)
	})
	c := newGroupsClient(t, h)
	got, err := c.Groups().Create(context.Background(), CreateGroupRequest{
		DisplayName: "Design Room",
		CreatorURN:  "msg://agent/agent-mux/agt_creator00",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got.URN != want.URN {
		t.Errorf("URN = %q want %q", got.URN, want.URN)
	}
}

// ─── Lookup ─────────────────────────────────────────────────────────────

func TestGroupsClient_Lookup_NotFound(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":{"code":"not_found","message":"absent"}}`))
	})
	c := newGroupsClient(t, h)
	_, err := c.Groups().Lookup(context.Background(), "msg://group/agent-mux/grp_aaaaaaaaaa")
	if !errors.Is(err, registry.ErrNotFound) {
		t.Errorf("err = %v, want errors.Is(ErrNotFound)", err)
	}
}

// ─── Send + ambiguous mention ───────────────────────────────────────────

func TestGroupsClient_Send_Ambiguous(t *testing.T) {
	// Server returns the ambiguous-mention 400 envelope.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{
			"error": {"code":"invalid_request","message":"ambiguous mention"},
			"token": "alice",
			"candidates": ["msg://agent/agent-mux/agt_aaaaaaaaaa","msg://agent/agent-mux/agt_bbbbbbbbbb"]
		}`))
	})
	c := newGroupsClient(t, h)
	_, err := c.Groups().Send(context.Background(),
		"msg://group/agent-mux/grp_aaaaaaaaaa",
		SendGroupRequest{From: "msg://agent/agent-mux/agt_caller", Payload: json.RawMessage(`{"text":"@alice"}`)})
	if err == nil {
		t.Fatal("expected error")
	}
	var amb *registry.ErrAmbiguousMention
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v, want errors.As(*ErrAmbiguousMention)", err)
	}
	if amb.Token != "alice" {
		t.Errorf("Token = %q want alice", amb.Token)
	}
	if len(amb.Candidates) != 2 {
		t.Errorf("Candidates = %v", amb.Candidates)
	}
}

// ─── Send: locked ───────────────────────────────────────────────────────

func TestGroupsClient_Send_Locked(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_, _ = w.Write([]byte(`{"error":{"code":"locked","message":"archived"}}`))
	})
	c := newGroupsClient(t, h)
	_, err := c.Groups().Send(context.Background(),
		"msg://group/agent-mux/grp_aaaaaaaaaa",
		SendGroupRequest{From: "msg://agent/agent-mux/agt_caller"})
	if !errors.Is(err, registry.ErrGroupArchived) {
		t.Errorf("err = %v, want errors.Is(ErrGroupArchived)", err)
	}
}

// ─── Send: forbidden ────────────────────────────────────────────────────

func TestGroupsClient_Send_Forbidden(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":"forbidden","message":"not a member"}}`))
	})
	c := newGroupsClient(t, h)
	_, err := c.Groups().Send(context.Background(),
		"msg://group/agent-mux/grp_aaaaaaaaaa",
		SendGroupRequest{From: "msg://agent/agent-mux/agt_caller"})
	if !errors.Is(err, registry.ErrForbidden) {
		t.Errorf("err = %v, want errors.Is(ErrForbidden)", err)
	}
}

// ─── ListMessages ───────────────────────────────────────────────────────

func TestGroupsClient_ListMessages_QueryParams(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("as") != "msg://agent/agent-mux/agt_caller" {
			t.Errorf("missing ?as: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("since_seq") != "5" {
			t.Errorf("missing/since_seq: %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("limit") != "10" {
			t.Errorf("missing/limit: %s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []registry.GroupMessage{},
			"next_seq": 5,
		})
	})
	c := newGroupsClient(t, h)
	out, err := c.Groups().ListMessages(context.Background(),
		"msg://group/agent-mux/grp_aaaaaaaaaa",
		ListMessagesParams{
			As:       "msg://agent/agent-mux/agt_caller",
			SinceSeq: 5,
			Limit:    10,
		})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	if out.Messages == nil {
		t.Errorf("nil messages slice")
	}
}

// ─── Mentions ────────────────────────────────────────────────────────────

func TestGroupsClient_Mentions_TimeFormat(t *testing.T) {
	wantSince := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mentions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.URL.Query().Get("since"); got != wantSince {
			t.Errorf("since = %q want %q", got, wantSince)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"mentions": []registry.GroupMessage{}})
	})
	c := newGroupsClient(t, h)
	out, err := c.Groups().Mentions(context.Background(), MentionsParams{
		As:    "msg://agent/agent-mux/agt_caller",
		Since: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("Mentions: %v", err)
	}
	if out == nil {
		t.Errorf("nil mentions slice")
	}
}

// ─── 500 surfaces body ──────────────────────────────────────────────────

func TestGroupsClient_Send_Error500(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"code":"internal_error","message":"kaboom"}}`))
	})
	c := newGroupsClient(t, h)
	_, err := c.Groups().Send(context.Background(),
		"msg://group/agent-mux/grp_aaaaaaaaaa",
		SendGroupRequest{From: "msg://agent/agent-mux/agt_caller"})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "kaboom") {
		t.Errorf("error missing body: %v", err)
	}
}
