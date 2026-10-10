package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	openai "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/responses"

	"github.com/hollis-labs/tether/internal/llm"
)

func TestProviderChatBuildsRequestAndTranslatesResponse(t *testing.T) {
	t.Parallel()

	var captured responses.ResponseNewParams
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient: func(apiKey string) responseClient {
			if apiKey != "sk-openai-test" {
				t.Fatalf("apiKey = %q", apiKey)
			}
			return stubResponseClient{
				newFn: func(_ context.Context, body responses.ResponseNewParams, _ ...option.RequestOption) (*responses.Response, error) {
					captured = body
					return mustResponse(t, `{
					  "id":"resp_123",
					  "created_at":1716595200,
					  "error":null,
					  "incomplete_details":null,
					  "instructions":null,
					  "metadata":{},
					  "model":"gpt-4o-mini",
					  "object":"response",
					  "output":[
					    {"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Calling tool","annotations":[],"logprobs":[]}]},
					    {"id":"fc_1","type":"function_call","status":"completed","call_id":"call_123","name":"weather","arguments":"{\"city\":\"Austin\"}"}
					  ],
					  "parallel_tool_calls":true,
					  "temperature":1,
					  "tool_choice":"auto",
					  "tools":[],
					  "top_p":1,
					  "background":false,
					  "max_output_tokens":321,
					  "service_tier":"default",
					  "status":"completed",
					  "text":{"format":{"type":"text"}},
					  "truncation":"disabled",
					  "usage":{
					    "input_tokens":12,
					    "input_tokens_details":{"cached_tokens":3},
					    "output_tokens":8,
					    "output_tokens_details":{"reasoning_tokens":2},
					    "total_tokens":20
					  }
					}`), nil
				},
			}
		},
	}

	resp, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{
			{Role: "system", Parts: []llm.ContentPart{{Type: "text", Text: "You are concise."}}},
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "Weather?"}}},
		},
		Tools: []llm.ToolDefinition{{
			Name:        "weather",
			Description: "Look up weather",
			SchemaJSON:  `{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`,
		}},
		MaxOutputTokens: 321,
	}, llm.RouteDecision{Provider: "openai-personal", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	t.Logf("resp: %+v", resp)

	raw, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal captured request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if got["model"] != "gpt-4o-mini" {
		t.Fatalf("captured model = %v", got["model"])
	}
	if got["max_output_tokens"] != float64(321) {
		t.Fatalf("captured max_output_tokens = %v", got["max_output_tokens"])
	}

	input, _ := got["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("captured input len = %d", len(input))
	}
	systemMsg, _ := input[0].(map[string]any)
	if systemMsg["role"] != "system" {
		t.Fatalf("captured first role = %v", systemMsg["role"])
	}
	tools, _ := got["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("captured tools = %v", got["tools"])
	}

	want := llm.Response{
		Provider:   "openai-personal",
		Model:      "gpt-4o-mini",
		StopReason: "tool_use",
		Usage: llm.Usage{
			InputTokens:     12,
			OutputTokens:    8,
			CacheReadTokens: 3,
			ReasoningTokens: 2,
		},
		Output: []llm.Message{
			{Role: "assistant", Parts: []llm.ContentPart{{Type: "text", Text: "Calling tool"}}},
			{Role: "assistant", ToolUse: &llm.ToolUse{Name: "weather", Arguments: `{"city":"Austin"}`, Invocation: "call_123"}},
		},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
}

func TestProviderChatRefusalUsesRefusalText(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient: func(string) responseClient {
			return stubResponseClient{
				newFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) (*responses.Response, error) {
					return mustResponse(t, `{
					  "id":"resp_123",
					  "created_at":1716595200,
					  "error":null,
					  "incomplete_details":null,
					  "instructions":null,
					  "metadata":{},
					  "model":"gpt-4o-mini",
					  "object":"response",
					  "output":[
					    {"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"refusal","refusal":"refused by policy"}]}
					  ],
					  "parallel_tool_calls":false,
					  "temperature":1,
					  "tool_choice":"auto",
					  "tools":[],
					  "top_p":1,
					  "background":false,
					  "service_tier":"default",
					  "status":"completed",
					  "text":{"format":{"type":"text"}},
					  "truncation":"disabled",
					  "usage":{
					    "input_tokens":1,
					    "input_tokens_details":{"cached_tokens":0},
					    "output_tokens":1,
					    "output_tokens_details":{"reasoning_tokens":0},
					    "total_tokens":2
					  }
					}`), nil
				},
			}
		},
	}

	resp, err := p.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "openai-personal", Model: "gpt-4o-mini"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	t.Logf("resp: %+v", resp)
	if resp.Refusal != "refused by policy" || resp.StopReason != "refusal" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestProviderChatRejectsUnsupportedPart(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient:     func(string) responseClient { return stubResponseClient{} },
	}

	_, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "audio"}},
		}},
	}, llm.RouteDecision{Provider: "openai-personal", Model: "gpt-4o-mini"})
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("Chat error = %v, want ErrUnsupportedInput", err)
	}
}

func TestProviderChatRequiresAPIKeyResolver(t *testing.T) {
	t.Parallel()

	p := &Provider{}
	_, err := p.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "openai-personal", Model: "gpt-4o-mini"})
	if !errors.Is(err, ErrAPIKeyResolverMissing) {
		t.Fatalf("Chat error = %v, want ErrAPIKeyResolverMissing", err)
	}
}

func TestProviderChatAllowsUnauthenticatedCompatibleServer(t *testing.T) {
	t.Parallel()

	var authHeader string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
		  "id":"resp_compat",
		  "created_at":1716595200,
		  "error":null,
		  "incomplete_details":null,
		  "instructions":null,
		  "metadata":{},
		  "model":"llama3.1",
		  "object":"response",
		  "output":[
		    {"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello local","annotations":[],"logprobs":[]}]}
		  ],
		  "parallel_tool_calls":false,
		  "temperature":1,
		  "tool_choice":"auto",
		  "tools":[],
		  "top_p":1,
		  "background":false,
		  "service_tier":"default",
		  "status":"completed",
		  "text":{"format":{"type":"text"}},
		  "truncation":"disabled",
		  "usage":{
		    "input_tokens":1,
		    "input_tokens_details":{"cached_tokens":0},
		    "output_tokens":1,
		    "output_tokens_details":{"reasoning_tokens":0},
		    "total_tokens":2
		  }
		}`))
	}))
	defer srv.Close()

	p := New(Config{
		BaseURL:              srv.URL + "/",
		HTTPClient:           srv.Client(),
		AllowUnauthenticated: true,
	})

	resp, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "text", Text: "hello"}},
		}},
	}, llm.RouteDecision{Provider: "llama-local", Model: "llama3.1"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	t.Logf("resp: %+v", resp)
	if authHeader != "" {
		t.Fatalf("Authorization header = %q, want empty", authHeader)
	}
	if resp.Provider != "llama-local" || resp.Model != "llama3.1" || len(resp.Output) != 1 {
		t.Fatalf("response = %+v", resp)
	}
}

func TestProviderEmbedBuildsRequestAndTranslatesResponse(t *testing.T) {
	t.Parallel()

	var captured openai.EmbeddingNewParams
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient: func(apiKey string) responseClient {
			if apiKey != "sk-openai-test" {
				t.Fatalf("apiKey = %q", apiKey)
			}
			return stubResponseClient{
				newEmbeddingFn: func(_ context.Context, body openai.EmbeddingNewParams, _ ...option.RequestOption) (*openai.CreateEmbeddingResponse, error) {
					captured = body
					return &openai.CreateEmbeddingResponse{
						Model: "text-embedding-3-small",
						Data: []openai.Embedding{
							{Index: 0, Embedding: []float64{0.1, 0.2}},
							{Index: 1, Embedding: []float64{0.3, 0.4}},
						},
						Usage: openai.CreateEmbeddingResponseUsage{PromptTokens: 11, TotalTokens: 11},
					}, nil
				},
			}
		},
	}

	resp, err := p.Embed(context.Background(), llm.Request{
		Operation:           llm.OperationEmbedding,
		EmbeddingInput:      []string{"alpha", "beta"},
		EmbeddingDimensions: 256,
		CallerID:            "nanite",
	}, llm.RouteDecision{Provider: "openai-personal", Model: "text-embedding-3-small"})
	if err != nil {
		t.Fatalf("Embed returned err: %v", err)
	}

	raw, err := json.Marshal(captured)
	if err != nil {
		t.Fatalf("marshal captured request: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal captured request: %v", err)
	}
	if got["model"] != "text-embedding-3-small" {
		t.Fatalf("captured model = %v", got["model"])
	}
	if got["dimensions"] != float64(256) {
		t.Fatalf("captured dimensions = %v", got["dimensions"])
	}
	input, _ := got["input"].([]any)
	if len(input) != 2 || input[0] != "alpha" || input[1] != "beta" {
		t.Fatalf("captured input = %#v", got["input"])
	}
	if got["user"] != "nanite" {
		t.Fatalf("captured user = %v", got["user"])
	}

	want := llm.Response{
		Provider: "openai-personal",
		Model:    "text-embedding-3-small",
		Usage:    llm.Usage{InputTokens: 11},
		Embeddings: []llm.Embedding{
			{Index: 0, Vector: []float64{0.1, 0.2}},
			{Index: 1, Vector: []float64{0.3, 0.4}},
		},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
}

func TestProviderStreamChatEmitsTextAndFinalResponse(t *testing.T) {
	t.Parallel()

	var seen []llm.StreamEvent
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient: func(string) responseClient {
			return stubResponseClient{
				newStreamFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) responseStream {
					return &stubOpenAIResponseStream{
						events: []responses.ResponseStreamEventUnion{
							mustResponseStreamEvent(t, `{"type":"response.output_text.delta","content_index":0,"delta":"hel","item_id":"msg_1","logprobs":[],"output_index":0,"sequence_number":1}`),
							mustResponseStreamEvent(t, `{"type":"response.output_text.delta","content_index":0,"delta":"lo","item_id":"msg_1","logprobs":[],"output_index":0,"sequence_number":2}`),
							mustResponseStreamEvent(t, `{"type":"response.completed","sequence_number":3,"response":{
							  "id":"resp_123",
							  "created_at":1716595200,
							  "error":null,
							  "incomplete_details":null,
							  "instructions":null,
							  "metadata":{},
							  "model":"gpt-4o-mini",
							  "object":"response",
							  "output":[
							    {"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[]}]}
							  ],
							  "parallel_tool_calls":false,
							  "temperature":1,
							  "tool_choice":"auto",
							  "tools":[],
							  "top_p":1,
							  "background":false,
							  "service_tier":"default",
							  "status":"completed",
							  "text":{"format":{"type":"text"}},
							  "truncation":"disabled",
							  "usage":{
							    "input_tokens":2,
							    "input_tokens_details":{"cached_tokens":0},
							    "output_tokens":1,
							    "output_tokens_details":{"reasoning_tokens":0},
							    "total_tokens":3
							  }
							}}`),
						},
					}
				},
			}
		},
	}

	resp, err := p.StreamChat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "openai-personal", Model: "gpt-4o-mini"}, func(ev llm.StreamEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat returned err: %v", err)
	}
	if len(seen) != 2 || seen[0].Delta != "hel" || seen[1].Delta != "lo" {
		t.Fatalf("seen = %+v", seen)
	}
	if resp.Model != "gpt-4o-mini" || len(resp.Output) != 1 || resp.Output[0].Parts[0].Text != "hello" {
		t.Fatalf("response = %+v", resp)
	}
}

func TestProviderChatFallsBackToChatCompletionsOn404(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		newClient: func(string) responseClient {
			return stubResponseClient{
				newFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) (*responses.Response, error) {
					return nil, &openai.Error{StatusCode: http.StatusNotFound}
				},
				newChatCompletionFn: func(_ context.Context, body openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
					raw, err := json.Marshal(body)
					if err != nil {
						t.Fatalf("marshal fallback body: %v", err)
					}
					var got map[string]any
					if err := json.Unmarshal(raw, &got); err != nil {
						t.Fatalf("unmarshal fallback body: %v", err)
					}
					if got["model"] != "llama3.1:8b" {
						t.Fatalf("fallback model = %v", got["model"])
					}
					return mustChatCompletion(t, `{
					  "id":"chatcmpl-638",
					  "object":"chat.completion",
					  "created":1716595200,
					  "model":"llama3.1:8b",
					  "choices":[{"index":0,"message":{"role":"assistant","content":"hello local"},"finish_reason":"stop"}],
					  "usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}
					}`), nil
				},
			}
		},
	}

	resp, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "text", Text: "say hello"}},
		}},
	}, llm.RouteDecision{Provider: "llama-local", Model: "llama3.1:8b"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	t.Logf("resp: %+v", resp)
	if resp.Provider != "llama-local" || resp.Model != "llama3.1:8b" || resp.Output[0].Parts[0].Text != "hello local" {
		t.Fatalf("response = %+v", resp)
	}
}

type stubResponseClient struct {
	newFn                   func(context.Context, responses.ResponseNewParams, ...option.RequestOption) (*responses.Response, error)
	newStreamFn             func(context.Context, responses.ResponseNewParams, ...option.RequestOption) responseStream
	newEmbeddingFn          func(context.Context, openai.EmbeddingNewParams, ...option.RequestOption) (*openai.CreateEmbeddingResponse, error)
	newChatCompletionFn     func(context.Context, openai.ChatCompletionNewParams, ...option.RequestOption) (*openai.ChatCompletion, error)
	newChatCompletionStream func(context.Context, openai.ChatCompletionNewParams, ...option.RequestOption) chatCompletionStream
}

func (s stubResponseClient) New(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) (*responses.Response, error) {
	if s.newFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newFn(ctx, body, opts...)
}

func (s stubResponseClient) NewStreaming(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) responseStream {
	if s.newStreamFn == nil {
		return &stubOpenAIResponseStream{err: errors.New("unexpected call")}
	}
	return s.newStreamFn(ctx, body, opts...)
}

func (s stubResponseClient) NewEmbedding(ctx context.Context, body openai.EmbeddingNewParams, opts ...option.RequestOption) (*openai.CreateEmbeddingResponse, error) {
	if s.newEmbeddingFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newEmbeddingFn(ctx, body, opts...)
}

func (s stubResponseClient) NewChatCompletion(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error) {
	if s.newChatCompletionFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newChatCompletionFn(ctx, body, opts...)
}

func (s stubResponseClient) NewChatCompletionStreaming(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) chatCompletionStream {
	if s.newChatCompletionStream == nil {
		return &stubOpenAIChatCompletionStream{err: errors.New("unexpected call")}
	}
	return s.newChatCompletionStream(ctx, body, opts...)
}

type stubOpenAIResponseStream struct {
	events []responses.ResponseStreamEventUnion
	err    error
	index  int
}

func (s *stubOpenAIResponseStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++
	return true
}

func (s *stubOpenAIResponseStream) Current() responses.ResponseStreamEventUnion {
	return s.events[s.index-1]
}

func (s *stubOpenAIResponseStream) Err() error { return s.err }

func (s *stubOpenAIResponseStream) Close() error { return nil }

func mustResponse(t *testing.T, raw string) *responses.Response {
	t.Helper()

	var resp responses.Response
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("Unmarshal(response): %v", err)
	}
	return &resp
}

func mustResponseStreamEvent(t *testing.T, raw string) responses.ResponseStreamEventUnion {
	t.Helper()

	var ev responses.ResponseStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("Unmarshal(response stream event): %v", err)
	}
	return ev
}

type stubOpenAIChatCompletionStream struct {
	events []openai.ChatCompletionChunk
	err    error
	index  int
}

func (s *stubOpenAIChatCompletionStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++
	return true
}

func (s *stubOpenAIChatCompletionStream) Current() openai.ChatCompletionChunk {
	return s.events[s.index-1]
}

func (s *stubOpenAIChatCompletionStream) Err() error { return s.err }

func (s *stubOpenAIChatCompletionStream) Close() error { return nil }

func mustChatCompletion(t *testing.T, raw string) *openai.ChatCompletion {
	t.Helper()

	var resp openai.ChatCompletion
	if err := json.Unmarshal([]byte(raw), &resp); err != nil {
		t.Fatalf("Unmarshal(chat completion): %v", err)
	}
	return &resp
}

func TestProviderChatPassesExtensionsAndTranslatesProvenance(t *testing.T) {
	t.Parallel()

	var capturedBody []byte
	var capturedHeader http.Header
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
		    "completion_tokens_details":{"reasoning_tokens": 5},
		    "cost": 0.005
		  },
		  "provider": "Anthropic"
		}`))
	}))
	t.Cleanup(srv.Close)

	p := New(Config{
		BaseURL:       srv.URL + "/v1/",
		ResolveAPIKey: func(context.Context) (string, error) { return "sk-openai-test", nil },
		OpenRouter: &OpenRouterExtension{
			Provider: map[string]any{"order": []string{"Anthropic"}},
		},
		HuggingFace: &HuggingFaceExtension{
			BillTo: "my-org",
		},
	})

	resp, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "text", Text: "say hello"}},
		}},
	}, llm.RouteDecision{Provider: "openrouter", Model: "auto"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	t.Logf("resp: %+v", resp)

	if resp.Usage.GenerationID != "gen-xyz" {
		t.Errorf("GenerationID = %v, want gen-xyz", resp.Usage.GenerationID)
	}
	if resp.Usage.BilledCostUSD == nil || *resp.Usage.BilledCostUSD != 0.005 {
		t.Errorf("BilledCostUSD = %v, want 0.005", resp.Usage.BilledCostUSD)
	}
	if resp.Usage.CostKind != "api_billed" {
		t.Errorf("CostKind = %q, want api_billed", resp.Usage.CostKind)
	}
	if resp.Usage.UpstreamProvider != "Anthropic" {
		t.Errorf("UpstreamProvider = %v, want Anthropic", resp.Usage.UpstreamProvider)
	}
	if resp.Usage.ReasoningTokens != 5 {
		t.Errorf("ReasoningTokens = %v, want 5", resp.Usage.ReasoningTokens)
	}

	if hf := capturedHeader.Get("X-HF-Bill-To"); hf != "my-org" {
		t.Errorf("X-HF-Bill-To = %q, want my-org", hf)
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

func TestProviderChatExplicitWireChatCompletions(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-test", nil },
		wire:          "chat_completions",
		newClient: func(string) responseClient {
			return stubResponseClient{
				newFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) (*responses.Response, error) {
					t.Fatalf("expected wire chat_completions to skip /responses")
					return nil, nil
				},
				newChatCompletionFn: func(_ context.Context, body openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
					return mustChatCompletion(t, `{
					  "id":"chatcmpl-638",
					  "object":"chat.completion",
					  "created":1716595200,
					  "model":"llama3.1:8b",
					  "choices":[{"index":0,"message":{"role":"assistant","content":"hello local"},"finish_reason":"stop"}],
					  "usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}
					}`), nil
				},
			}
		},
	}

	req := llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "Hi"}}},
		},
	}
	route := llm.RouteDecision{Model: "llama3.1:8b"}

	resp, err := p.Chat(context.Background(), req, route)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(resp.Output) == 0 || resp.Output[0].Parts[0].Text != "hello local" {
		t.Errorf("unexpected response: %+v", resp.Output)
	}
}

func TestProviderChatExplicitWireResponses(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-test", nil },
		wire:          "responses",
		newClient: func(string) responseClient {
			return stubResponseClient{
				newFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) (*responses.Response, error) {
					return nil, &openai.Error{StatusCode: http.StatusNotFound}
				},
				newChatCompletionFn: func(_ context.Context, body openai.ChatCompletionNewParams, _ ...option.RequestOption) (*openai.ChatCompletion, error) {
					t.Fatalf("expected wire responses to skip chat_completions fallback")
					return nil, nil
				},
			}
		},
	}

	req := llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "Hi"}}},
		},
	}
	route := llm.RouteDecision{Model: "llama3.1:8b"}

	_, err := p.Chat(context.Background(), req, route)
	if err == nil {
		t.Fatalf("expected error from 404")
	}
}

func TestProviderStreamChatExplicitWireChatCompletions(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-test", nil },
		wire:          "chat_completions",
		newClient: func(string) responseClient {
			return stubResponseClient{
				newStreamFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) responseStream {
					t.Fatalf("expected wire chat_completions to skip /responses")
					return nil
				},
				newChatCompletionStream: func(_ context.Context, body openai.ChatCompletionNewParams, _ ...option.RequestOption) chatCompletionStream {
					return &stubOpenAIChatCompletionStream{
						events: []openai.ChatCompletionChunk{
							func() openai.ChatCompletionChunk {
								var chunk openai.ChatCompletionChunk
								json.Unmarshal([]byte(`{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"hello local"}}],"usage":null}`), &chunk)
								return chunk
							}(),
						},
					}
				},
			}
		},
	}

	req := llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "Hi"}}},
		},
	}
	route := llm.RouteDecision{Model: "llama3.1:8b"}

	_, err := p.StreamChat(context.Background(), req, route, func(ev llm.StreamEvent) error { return nil })
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
}

func TestProviderStreamChatExplicitWireResponses(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-test", nil },
		wire:          "responses",
		newClient: func(string) responseClient {
			return stubResponseClient{
				newStreamFn: func(context.Context, responses.ResponseNewParams, ...option.RequestOption) responseStream {
					return &stubOpenAIResponseStream{err: &openai.Error{StatusCode: http.StatusNotFound}}
				},
				newChatCompletionStream: func(_ context.Context, body openai.ChatCompletionNewParams, _ ...option.RequestOption) chatCompletionStream {
					t.Fatalf("expected wire responses to skip chat_completions fallback")
					return nil
				},
			}
		},
	}

	req := llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "Hi"}}},
		},
	}
	route := llm.RouteDecision{Model: "llama3.1:8b"}

	_, err := p.StreamChat(context.Background(), req, route, func(ev llm.StreamEvent) error { return nil })
	if err == nil {
		t.Fatalf("expected error from 404")
	}
}
