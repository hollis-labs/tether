package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/llm"
	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
)

func TestConfiguredHeadersReachProvider(t *testing.T) {
	for name, build := range map[string]func(config.AIProviderConfig) llm.ChatProvider{
		"openai": func(p config.AIProviderConfig) llm.ChatProvider {
			return llmopenai.New(openAIAdapterConfig(p, func(context.Context) (string, error) { return "k", nil }))
		},
		"openai-compatible": func(p config.AIProviderConfig) llm.ChatProvider {
			return llmopenai.New(llmopenai.Config{
				BaseURL:              p.BaseURL,
				Wire:                 p.Wire,
				Headers:              openAICompatAdapterConfig(p, nil).Headers,
				AllowUnauthenticated: true,
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			var got http.Header
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got = r.Header
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
			}))
			defer srv.Close()

			p := config.AIProviderConfig{
				ID: "go", Type: name, BaseURL: srv.URL + "/v1", Wire: "chat_completions", Models: []string{"m"},
				Headers: map[string]string{"x-opencode-session": "tether"},
			}
			_, err := build(p).Chat(context.Background(), llm.Request{
				Operation: llm.OperationChat,
				Input:     []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hi"}}}},
			}, llm.RouteDecision{Provider: "go", Model: "m"})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if v := got.Get("X-Opencode-Session"); v != "tether" {
				t.Fatalf("x-opencode-session = %q", v)
			}
		})
	}
}
