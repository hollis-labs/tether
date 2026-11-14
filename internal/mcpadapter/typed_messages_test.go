package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

type typedCatalog struct{ cat *config.Catalog }

func (c typedCatalog) Load() (*config.Catalog, error) { return c.cat, nil }

func newTypedAdapter(t *testing.T) *Adapter {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "typed.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cat := &config.Catalog{Agents: map[string]config.Agent{
		"photon-tether": {ID: "photon-tether", URN: "msg://agent/agent-mux/agt_me"},
		"archie":        {ID: "archie", URN: "msg://agent/agent-mux/agt_archie"},
	}}
	srv := httptest.NewServer(api.NewHandler(api.Deps{MessageStore: db.MessagingStore(), Seats: db, Catalog: typedCatalog{cat}}))
	t.Cleanup(srv.Close)
	dc := client.New("tcp:" + strings.TrimPrefix(srv.URL, "http://"))
	return NewWithDaemon(&app.Service{Store: db}, dc, "test-token", []string{ScopeMessageWrite})
}

func toolOut(t *testing.T, res any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	var wrap struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &wrap) == nil && len(wrap.Content) > 0 && wrap.Content[0].Text != "" {
		raw = []byte(wrap.Content[0].Text)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func TestTypedMessageToolsRoundTrip(t *testing.T) {
	a := newTypedAdapter(t)
	ctx := context.Background()
	res, err := a.handleMessagePost(ctx, map[string]any{
		"as": "msg://agent/agent-mux/agt_me", "kind": "decision", "to": []any{"@archie"}, "subject": "Stop gate PR split",
		"body": "Ship the gate as a second PR?", "options": []any{map[string]any{"id": "one", "label": "One PR"}, map[string]any{"id": "two", "label": "Two PRs", "recommended": true}},
		"default_option": "two",
	})
	if err != nil {
		t.Fatal(err)
	}
	out := toolOut(t, res)
	ids, _ := out["ids"].([]any)
	if out["ok"] != true || len(ids) != 1 || out["thread_id"] == "" {
		t.Fatalf("post = %v", out)
	}
	res, err = a.handlePendingReplies(ctx, map[string]any{"as": "msg://agent/agent-mux/agt_archie"})
	if err != nil {
		t.Fatal(err)
	}
	if pending, _ := toolOut(t, res)["pending"].([]any); len(pending) != 1 {
		t.Fatalf("pending = %v", toolOut(t, res))
	}

	// @unknown is refused, naming the field.
	_, err = a.handleMessagePost(ctx, map[string]any{"as": "msg://agent/agent-mux/agt_me", "kind": "message", "to": "@unknown", "body": "hi"})
	if err == nil || !strings.Contains(err.Error(), "unknown handle @unknown") {
		t.Fatalf("unknown handle: %v", err)
	}

	// Status: blocked needs an owner; with one, the card shows it.
	if _, err = a.handleStatusSet(ctx, map[string]any{"as": "msg://agent/agent-mux/agt_me", "state": "blocked"}); err == nil || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("blocked without owner: %v", err)
	}
	if _, err = a.handleStatusSet(ctx, map[string]any{"as": "msg://agent/agent-mux/agt_me", "state": "blocked", "owner": "@archie", "reason": "waiting on the harness tag", "ttl_seconds": float64(3600)}); err != nil {
		t.Fatal(err)
	}
	res, err = a.handleStatusGet(ctx, map[string]any{"handle": "@photon-tether"})
	if err != nil {
		t.Fatal(err)
	}
	card := toolOut(t, res)
	declared, _ := card["declared"].(map[string]any)
	if card["effective"] != "blocked" || declared["owner"] != "@archie" {
		t.Fatalf("card = %v", card)
	}
}
