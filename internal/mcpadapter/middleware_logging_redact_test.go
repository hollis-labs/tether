package mcpadapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/redact"
)

// endEventOf runs one call through m and returns the tool_call_end event it
// published, which is what ToolCallEventStore and proxy_events persist.
func endEventOf(t *testing.T, m *LoggingMiddleware, bus events.Bus, next ToolCallHandler) events.ToolCallEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ch, unsub, err := bus.Subscribe(ctx, events.Filter{})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer unsub()

	_, _ = m.Handle(ctx, ToolCall{ToolName: "upstream_tool", Args: map[string]any{"token": "x"}}, next)

	for {
		select {
		case ev := <-ch:
			if ev.Kind != events.EventTypeToolCallEnd {
				continue
			}
			var tce events.ToolCallEvent
			if err := json.Unmarshal([]byte(ev.PayloadJSON), &tce); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			return tce
		case <-ctx.Done():
			t.Fatal("no tool_call_end event published")
		}
	}
}

// An error carrying a configured secret is scrubbed before the event is
// published, on both paths: a transport error and an upstream's IsError
// text (CW-20260930-0009).
func TestLoggingMiddleware_RedactsSecretsInErrorText(t *testing.T) {
	const secret = "oauth-token-0123456789"
	secrets := &redact.Set{}
	secrets.Add(secret)
	bus := events.NewBus(events.BusOptions{Persister: &fakeEventPersister{}})
	m := NewLoggingMiddleware(bus).RedactWith(secrets)

	ev := endEventOf(t, m, bus, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) {
		return nil, errors.New("upstream rejected token " + secret)
	})
	if strings.Contains(ev.Error, secret) || ev.Error != "upstream rejected token [redacted]" {
		t.Fatalf("transport error persisted as %q", ev.Error)
	}

	ev = endEventOf(t, m, bus, func(context.Context, ToolCall) (*mcpsdk.CallToolResult, error) {
		return &mcpsdk.CallToolResult{IsError: true, Content: []mcpsdk.Content{
			&mcpsdk.TextContent{Text: `invalid argument "token": "` + secret + `"`},
		}}, nil
	})
	if strings.Contains(ev.Error, secret) || ev.Error != `invalid argument "token": "[redacted]"` {
		t.Fatalf("upstream error text persisted as %q", ev.Error)
	}
}

// The proxy's set covers every configured server's token, env values and
// substituted argument values, so one server's credential echoed by
// another server's tool is still scrubbed.
func TestProxyRedactionSet_CoversEveryServer(t *testing.T) {
	t.Setenv("CW_0009_ARG_SECRET", "arg-secret-value-42")
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, "mcp-servers"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mcp-servers", name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("a.yaml", "id: a\ntransport: stdio\ncommand: echo\nargs: [\"--key=${CW_0009_ARG_SECRET}\"]\nenv:\n  API_KEY: env-secret-value-1\n")
	write("b.yaml", "id: b\ntransport: http\nurl: http://127.0.0.1:1\ntoken: bearer-secret-value-2\n")
	entries, err := config.LoadMCPServers(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	got := proxyRedactionSet(entries).Redact("a=arg-secret-value-42 b=env-secret-value-1 c=bearer-secret-value-2")
	if got != "a=[redacted] b=[redacted] c=[redacted]" {
		t.Fatalf("Redact = %q", got)
	}
}
