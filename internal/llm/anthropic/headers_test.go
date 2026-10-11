package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/llm"
)

func TestConfiguredHeadersReachAnthropicGateway(t *testing.T) {
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","model":"x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	p := New(Config{
		BaseURL:       srv.URL,
		ResolveAPIKey: func(context.Context) (string, error) { return "k", nil },
		Headers:       map[string]string{"x-opencode-session": "tether"},
	})
	_, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input:     []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hi"}}}},
	}, llm.RouteDecision{Provider: "a", Model: "x"})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if v := got.Get("X-Opencode-Session"); v != "tether" {
		t.Fatalf("x-opencode-session = %q", v)
	}
}
