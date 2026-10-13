package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	forumhost "github.com/hollis-labs/tether/internal/forums"
	"github.com/hollis-labs/tether/internal/identity"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestForumToolsRouteThroughCredentialedDaemon(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	ids := identity.NewStore(a.svc.Store.DB())
	token, e := ids.Mint(context.Background(), identity.Principal{ID: identity.OperatorID, Kind: "operator", Scopes: []string{"*"}})
	if e != nil {
		t.Fatal(e)
	}
	server := httptest.NewServer((&daemon.Server{Config: daemon.Config{IdentityMode: identity.Observe}, Identity: ids, Forums: forumhost.New(a.svc.Store)}).Handler())
	t.Cleanup(server.Close)
	a.client = client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken(token))
	s := a.newBareServer()
	a.registerForumTools(s)
	consumer := connectInMemory(t, s)
	call := func(name string, args map[string]any) map[string]any {
		t.Helper()
		r, e := consumer.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: name, Arguments: args})
		if e != nil || r.IsError {
			t.Fatalf("%s: %v %v", name, r, e)
		}
		var out map[string]any
		for _, c := range r.Content {
			if text, ok := c.(*mcpsdk.TextContent); ok {
				if e = json.Unmarshal([]byte(text.Text), &out); e != nil {
					t.Fatal(e)
				}
			}
		}
		return out
	}
	call("tether_forum_create", map[string]any{"id": "portfolio", "namespace": "app/torque/forum/portfolio", "title": "Portfolio"})
	call("tether_forum_participant_add", map[string]any{"forum_id": "portfolio", "principal_kind": "agent", "principal_id": "msg://agent/env/pm", "access": "post"})
	grants := call("tether_forum_participant_list", map[string]any{"forum_id": "portfolio", "limit": 1})
	if len(grants["items"].([]any)) != 1 {
		t.Fatal("participant management did not route through daemon")
	}
	call("tether_forum_owner_set", map[string]any{"forum_id": "portfolio"})
	call("tether_forum_participant_revoke", map[string]any{"forum_id": "portfolio", "principal_kind": "agent", "principal_id": "msg://agent/env/pm"})
	if len(call("tether_forum_participant_list", map[string]any{"forum_id": "portfolio"})["items"].([]any)) != 0 {
		t.Fatal("grant not revoked")
	}
	topic := call("tether_forum_topic_create", map[string]any{"forum_id": "portfolio", "title": "Review"})["topic"].(map[string]any)["id"].(string)
	post := call("tether_forum_post_create", map[string]any{"topic_id": topic, "body_md": strings.Repeat("hello ", 1000)})["post"].(map[string]any)["id"].(string)
	pg := call("tether_forum_topic_get", map[string]any{"topic_id": topic})["outline"].(map[string]any)
	if p := pg["items"].([]any)[0].(map[string]any); p["body_md"] != nil || len(p["snippet"].(string)) > 160 {
		t.Fatal("outline was not compact")
	}
	full := call("tether_forum_post_get", map[string]any{"post_id": post, "detail": "full", "max_chars": 20})["post"].(map[string]any)
	if len(full["body_md"].(string)) != 20 || !full["truncated"].(bool) {
		t.Fatal("MCP body budget was not honored")
	}
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"tether_forum_post_create", map[string]any{"topic_id": topic, "body_md": "spoof", "provenance": map[string]any{"id": "spoofed"}}},
		{"tether_forum_post_list", map[string]any{"topic_id": topic, "limit": 1.5}},
	} {
		r, e := consumer.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		if e == nil && !r.IsError {
			t.Fatalf("invalid MCP request allowed: %s", tc.name)
		}
	}
	call("tether_forum_search", map[string]any{"forum_id": "portfolio", "query": "hello"})
	a.client = client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken(""))
	r, e := consumer.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: "tether_forum_list", Arguments: map[string]any{}})
	if e == nil && !r.IsError {
		t.Fatal("anonymous MCP read bypassed forum policy")
	}
}
