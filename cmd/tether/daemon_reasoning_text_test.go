package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/llm"
	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
	llmopenaicompat "github.com/hollis-labs/tether/internal/llm/openaicompat"
)

func TestDaemonReasoningTextFlagReachesOpenAIProviders(t *testing.T) {
	tests := []struct {
		name string
		typ  string
		new  func(config.AIProviderConfig) llm.ChatProvider
	}{
		{
			name: "openai",
			typ:  "openai",
			new: func(p config.AIProviderConfig) llm.ChatProvider {
				return llmopenai.New(openAIAdapterConfig(p, func(context.Context) (string, error) {
					return "test-key", nil
				}))
			},
		},
		{
			name: "openai-compatible",
			typ:  "openai-compatible",
			new: func(p config.AIProviderConfig) llm.ChatProvider {
				return llmopenaicompat.New(openAICompatAdapterConfig(p, nil))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/chat/completions" {
					t.Errorf("path = %q, want /chat/completions", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{
				  "id":"chatcmpl-daemon-reasoning",
				  "object":"chat.completion",
				  "created":1716595200,
				  "model":"reasoner",
				  "choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"daemon wiring reached provider","content":"answer"},"finish_reason":"stop"}],
				  "usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}
				}`))
			}))
			defer srv.Close()

			providerConfig := config.AIProviderConfig{
				ID:                   tc.name,
				Type:                 tc.typ,
				BaseURL:              srv.URL + "/",
				Wire:                 "chat_completions",
				IncludeReasoningText: true,
			}
			provider := tc.new(providerConfig)
			resp, err := provider.Chat(context.Background(), llm.Request{
				Operation: llm.OperationChat,
				Input:     []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "answer"}}}},
			}, llm.RouteDecision{Provider: tc.name, Model: "reasoner"})
			if err != nil {
				t.Fatalf("Chat: %v", err)
			}
			if len(resp.Output) != 1 || len(resp.Output[0].Parts) != 2 {
				t.Fatalf("output = %+v, want reasoning and text parts", resp.Output)
			}
			if got := resp.Output[0].Parts[0]; got.Type != "reasoning" || got.Text != "daemon wiring reached provider" {
				t.Fatalf("reasoning part = %+v", got)
			}
		})
	}
}
