package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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
				newFn: func(_ context.Context, body responses.ResponseNewParams) (*responses.Response, error) {
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
				newFn: func(context.Context, responses.ResponseNewParams) (*responses.Response, error) {
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
				newEmbeddingFn: func(_ context.Context, body openai.EmbeddingNewParams) (*openai.CreateEmbeddingResponse, error) {
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
				newStreamFn: func(context.Context, responses.ResponseNewParams) responseStream {
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

type stubResponseClient struct {
	newFn          func(context.Context, responses.ResponseNewParams) (*responses.Response, error)
	newStreamFn    func(context.Context, responses.ResponseNewParams) responseStream
	newEmbeddingFn func(context.Context, openai.EmbeddingNewParams) (*openai.CreateEmbeddingResponse, error)
}

func (s stubResponseClient) New(ctx context.Context, body responses.ResponseNewParams, _ ...option.RequestOption) (*responses.Response, error) {
	if s.newFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newFn(ctx, body)
}

func (s stubResponseClient) NewStreaming(ctx context.Context, body responses.ResponseNewParams, _ ...option.RequestOption) responseStream {
	if s.newStreamFn == nil {
		return &stubOpenAIResponseStream{err: errors.New("unexpected call")}
	}
	return s.newStreamFn(ctx, body)
}

func (s stubResponseClient) NewEmbedding(ctx context.Context, body openai.EmbeddingNewParams, _ ...option.RequestOption) (*openai.CreateEmbeddingResponse, error) {
	if s.newEmbeddingFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newEmbeddingFn(ctx, body)
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
