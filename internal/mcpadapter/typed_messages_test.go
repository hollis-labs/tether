package mcpadapter

import (
	"context"

	"encoding/json"
	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
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

// The raw send tools point a seat at tether_message_post when it passes a
// top-level body or a typed kind like "answer" (CW-20261114-0013).
func TestRawSendErrorsPointAtTheTypedTool(t *testing.T) {
	a := newTypedAdapter(t)
	ctx := context.Background()
	send := strictArgs(gomcp.Tool{Name: "tether_message_send", InputSchema: gomcp.InputSchema(
		gomcp.StringProp("from", "", true), gomcp.StringProp("to", "", true), gomcp.StringProp("kind", "", true), gomcp.StringProp("payload_json", "", false)),
		Handler: a.handleMessageSend})
	for name, args := range map[string]map[string]any{
		"body arg":    {"from": "msg://agent/agent-mux/agt_me", "to": "msg://agent/agent-mux/agt_archie", "kind": "notice", "body": "hi"},
		"answer kind": {"from": "msg://agent/agent-mux/agt_me", "to": "msg://agent/agent-mux/agt_archie", "kind": "answer"},
	} {
		_, err := send.Handler(ctx, args)
		if err == nil || !strings.Contains(err.Error(), "use tether_message_post") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// CW-20261117-0003: tether_ask / tether_reply build, validate and address the
// typed envelope; the reply goes back to the asker and awaits them, without
// holding either side's Stop gate.
func TestAskReplyRoundTrip(t *testing.T) {
	a := newTypedAdapter(t)
	ctx := context.Background()
	const me, archie = "msg://agent/agent-mux/agt_me", "msg://agent/agent-mux/agt_archie"

	// A request needs a deliverable; the refusal names the field.
	if _, err := a.handleAsk(ctx, map[string]any{"as": me, "kind": "request", "to": []any{"@archie"}, "subject": "Land 0002", "body": "please"}); err == nil || !strings.Contains(err.Error(), "deliverable") {
		t.Fatalf("request without deliverable: %v", err)
	}
	res, err := a.handleAsk(ctx, map[string]any{"as": me, "kind": "request", "to": []any{"@archie"}, "subject": "Land 0002", "body": "please", "deliverable": "Merged PR and deploy event", "re": "task:CW-20261117-0002"})
	if err != nil {
		t.Fatal(err)
	}
	ask := toolOut(t, res)
	askID := ask["ids"].([]any)[0].(string)
	if env, _ := ask["envelope"].(map[string]any); env["kind"] != "request" || env["deliverable"] != "Merged PR and deploy event" || env["from"] != "@photon-tether" {
		t.Fatalf("ask envelope = %v", ask["envelope"])
	}
	res, _ = a.handlePendingReplies(ctx, map[string]any{"as": archie})
	if pending, _ := toolOut(t, res)["pending"].([]any); len(pending) != 1 {
		t.Fatalf("archie pending = %v", toolOut(t, res))
	}

	// The reply needs no to: it goes to the asker and awaits them.
	res, err = a.handleReply(ctx, map[string]any{"as": archie, "in_reply_to": askID, "body": "Landed.", "status": "done", "evidence": []any{"https://github.com/hollis-labs/tether/pull/606"}})
	if err != nil {
		t.Fatal(err)
	}
	reply, _ := toolOut(t, res)["envelope"].(map[string]any)
	to, _ := reply["to"].([]any)
	if len(to) != 1 || to[0] != "@photon-tether" || reply["awaiting"] != "@photon-tether" || reply["in_reply_to"] != askID || reply["status"] != "done" {
		t.Fatalf("reply envelope = %v", reply)
	}
	// Answered: archie owes nothing; the reply does not hold the asker's gate either.
	for _, as := range []string{archie, me} {
		res, _ = a.handlePendingReplies(ctx, map[string]any{"as": as})
		if pending, _ := toolOut(t, res)["pending"].([]any); len(pending) != 0 {
			t.Fatalf("%s pending after the reply = %v", as, pending)
		}
	}

	// An answer must be one of the ask's options.
	res, err = a.handleAsk(ctx, map[string]any{"as": me, "kind": "decision", "to": []any{"@archie"}, "subject": "Split?", "body": "One PR or two?",
		"options": []any{map[string]any{"id": "one", "label": "One"}, map[string]any{"id": "two", "label": "Two"}}})
	if err != nil {
		t.Fatal(err)
	}
	decID := toolOut(t, res)["ids"].([]any)[0].(string)
	if _, err := a.handleReply(ctx, map[string]any{"as": archie, "in_reply_to": decID, "body": "three", "answer": "three"}); err == nil || !strings.Contains(err.Error(), "answer.option") {
		t.Fatalf("answer outside the options: %v", err)
	}
	if _, err := a.handleReply(ctx, map[string]any{"as": archie, "in_reply_to": decID, "body": "two", "answer": "two", "note": "smaller diffs"}); err != nil {
		t.Fatalf("valid answer refused: %v", err)
	}
	// status without in_reply_to cannot be built by tether_reply, but a post is refused.
	if _, err := a.handleMessagePost(ctx, map[string]any{"as": me, "kind": "message", "to": "@archie", "body": "x", "status": "done"}); err == nil || !strings.Contains(err.Error(), "status") {
		t.Fatalf("status without in_reply_to: %v", err)
	}
}

// TestMessagePostSchemaDeclaresEveryEnvelopeField pins CW-20261118-0024:
// strictArgs refuses undeclared arguments, so every field the handler forwards
// must be in the schema. deliverable was missing, which made kind request
// impossible to send through tether_message_post.
func TestMessagePostSchemaDeclaresEveryEnvelopeField(t *testing.T) {
	declared := declaredArgNames(messagePostInputSchema())
	for _, f := range append([]string{"as", "urgency", "idempotency_key"}, envelopeFields...) {
		if _, ok := declared[f]; !ok {
			t.Errorf("tether_message_post schema does not declare %q", f)
		}
	}
}

func TestMessagePostSendsRequestWithDeliverable(t *testing.T) {
	a := newTypedAdapter(t)
	ctx := context.Background()
	post := strictArgs(gomcp.Tool{Name: "tether_message_post", InputSchema: messagePostInputSchema(), Handler: a.handleMessagePost})
	res, err := post.Handler(ctx, map[string]any{
		"as": "msg://agent/agent-mux/agt_me", "kind": "request", "to": []any{"@archie"}, "re": "task:CW-20261118-0024",
		"attention": "agent", "subject": "Lead idle", "body": "You have been idle for 30 minutes.", "deliverable": "A reply with status done, declined or blocked",
	})
	if err != nil {
		t.Fatal(err)
	}
	env, _ := toolOut(t, res)["envelope"].(map[string]any)
	if env["deliverable"] != "A reply with status done, declined or blocked" || env["kind"] != "request" {
		t.Fatalf("envelope = %v", env)
	}
	if _, err := post.Handler(ctx, map[string]any{"as": "msg://agent/agent-mux/agt_me", "kind": "request", "to": []any{"@archie"}, "subject": "x", "body": "x"}); err == nil || !strings.Contains(err.Error(), "deliverable") {
		t.Fatalf("request without deliverable: %v", err)
	}
}
