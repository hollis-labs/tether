package mcpadapter

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	gomsg "github.com/hollis-labs/go-messaging"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestChannelToolsUseDaemonAndDurableCursors(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	a := newTestAdapter(t)
	channelService := channels.New(a.svc.Store, nil)
	from, err := gomsg.ParseURN("msg://agent/local/publisher")
	if err != nil {
		t.Fatal(err)
	}
	to, err := channels.ChannelAddress("ops")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ops", "other"} {
		address, err := channels.ChannelAddress(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := channelService.Publish(context.Background(), gomsg.Envelope{From: from, To: address, Kind: gomsg.MsgKindNotice, Payload: json.RawMessage(`"hello"`)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := channelService.Publish(context.Background(), gomsg.Envelope{From: from, To: to, Kind: gomsg.MsgKindNotice, Payload: json.RawMessage(`"latest"`)}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&daemon.Server{Channels: channelService, Routing: a.svc}).Handler())
	t.Cleanup(server.Close)
	a.client = client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken(""))
	s := a.newBareServer()
	a.registerChannelTools(s)
	consumer := connectInMemory(t, s)
	tools, err := consumer.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.DestructiveHint == nil || *tool.Annotations.DestructiveHint {
			t.Fatalf("annotations for %s = %+v", tool.Name, tool.Annotations)
		}
	}
	for _, tc := range []struct {
		name      string
		args      map[string]any
		wantError bool
		contains  string
	}{
		{"tether_channel_list", map[string]any{"as": "msg://user/local/consumer", "limit": 1}, false, `"next_offset":1`},
		{"tether_channel_list", map[string]any{"as": "msg://user/local/consumer", "offset": 1, "limit": 1}, false, `"other"`},
		{"tether_channel_read", map[string]any{"name": "ops", "as": "msg://user/local/consumer", "last": 1}, false, `latest`},
		{"tether_channel_read", map[string]any{"name": "ops", "as": "msg://user/local/consumer", "since": 1}, false, `latest`},
		{"tether_channel_read", map[string]any{"name": "ops", "as": "msg://user/local/consumer", "since": -1}, true, `invalid_request`},
		{"tether_channel_read", map[string]any{"name": "ops", "as": "msg://user/local/consumer", "last": 1, "limit": 1}, true, `invalid_request`},
		{"tether_routing_get", map[string]any{}, false, `next-turn`},
		{"tether_routing_get", map[string]any{"session_id": "missing"}, true, `not_found`},
	} {
		r, err := consumer.CallTool(context.Background(), &mcpsdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil {
			t.Fatal(err)
		}
		if r.IsError != tc.wantError {
			t.Fatalf("%s %+v = %+v", tc.name, tc.args, r)
		}
		b, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), tc.contains) {
			t.Fatalf("%s result = %s, want %s", tc.name, b, tc.contains)
		}
	}
}

func TestChannelNativeToolsUseVerifiedPrincipal(t *testing.T) {
	a := newTestAdapter(t)
	a.principal = &identity.Principal{ID: "msg://user/local/verified"}
	if _, err := a.handleChannelRead(context.Background(), map[string]any{"name": "ops", "as": "invalid"}); err != nil {
		t.Fatal(err)
	}
	a.principal = nil
	if _, err := a.handleChannelRead(context.Background(), map[string]any{"name": "ops", "as": "msg://user/local/asserted"}); err == nil {
		t.Fatal("standalone read bypassed daemon")
	}
}
