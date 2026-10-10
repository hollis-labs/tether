package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/llm"
	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
)

func TestDaemonExtensionsReachProvider(t *testing.T) {
	var capturedHeader http.Header
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "chat/completions") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		capturedHeader = r.Header
		capturedBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
		  "id":"gen-xyz",
		  "object":"chat.completion",
		  "created":1716595200,
		  "model":"anthropic/claude-3-sonnet",
		  "choices":[{"index":0,"message":{"role":"assistant","content":"hello extensions"},"finish_reason":"stop"}],
		  "usage":{
		    "prompt_tokens":10,
		    "completion_tokens":5,
		    "total_tokens":15,
		    "cost": 0.005
		  },
		  "provider": "Anthropic"
		}`))
	}))
	t.Cleanup(srv.Close)

	p := config.AIProviderConfig{
		ID:        "test-openrouter",
		Type:      "openai",
		BaseURL:   srv.URL + "/v1/",
		Models:    []string{"auto"},
		SecretRef: "literal://noop",
		Enabled:   true,
		Extensions: &config.ProviderExtensions{
			OpenRouter: &config.OpenRouterExtension{
				Provider: map[string]any{"order": []any{"Anthropic"}},
			},
			HuggingFace: &config.HuggingFaceExtension{
				BillTo: "test-org",
			},
		},
	}

	openaiCfg := llmopenai.Config{
		BaseURL:       p.BaseURL,
		ResolveAPIKey: func(context.Context) (string, error) { return "noop", nil },
		OpenRouter:    mapOpenRouterExt(p.Extensions),
		HuggingFace:   mapHuggingFaceExt(p.Extensions),
	}

	client := llmopenai.New(openaiCfg)

	req := llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "text", Text: "say hello"}},
		}},
	}
	route := llm.RouteDecision{Provider: "test-openrouter", Model: "auto"}

	_, err := client.Chat(context.Background(), req, route)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if hf := capturedHeader.Get("X-HF-Bill-To"); hf != "test-org" {
		t.Errorf("X-HF-Bill-To = %q, want test-org", hf)
	}
	var reqBody struct {
		Provider map[string]any `json:"provider"`
	}
	if err := json.Unmarshal(capturedBody, &reqBody); err != nil {
		t.Fatalf("decode request body: %v", err)
	}
	if order, ok := reqBody.Provider["order"].([]any); !ok || len(order) == 0 || order[0] != "Anthropic" {
		t.Errorf("provider JSON = %v, want {order: [Anthropic]}", reqBody.Provider)
	}
}
