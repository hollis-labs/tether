package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/hollis-labs/tether/internal/llm"
)

func TestProviderChatBuildsRequestAndTranslatesResponse(t *testing.T) {
	t.Parallel()

	var captured sdk.MessageNewParams
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-ant-test", nil },
		newClient: func(apiKey string) messageClient {
			if apiKey != "sk-ant-test" {
				t.Fatalf("apiKey = %q", apiKey)
			}
			return stubMessageClient{
				newFn: func(_ context.Context, body sdk.MessageNewParams) (*sdk.Message, error) {
					captured = body
					return &sdk.Message{
						Model:      "claude-sonnet-4-5",
						StopReason: sdk.StopReasonToolUse,
						Content: []sdk.ContentBlockUnion{
							mustContentBlock(t, `{"type":"text","text":"Calling tool"}`),
							mustContentBlock(t, `{"type":"tool_use","id":"toolu_123","name":"weather","input":{"city":"Austin"},"caller":{"type":"direct"}}`),
						},
						Usage: sdk.Usage{
							InputTokens:              12,
							OutputTokens:             8,
							CacheCreationInputTokens: 3,
							CacheReadInputTokens:     1,
						},
					}, nil
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
	}, llm.RouteDecision{Provider: "anthropic-work", Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}

	if captured.Model != "claude-sonnet-4-5" || captured.MaxTokens != 321 {
		t.Fatalf("captured model/max_tokens = %q/%d", captured.Model, captured.MaxTokens)
	}
	if len(captured.System) != 1 || captured.System[0].Text != "You are concise." {
		t.Fatalf("captured system = %+v", captured.System)
	}
	if len(captured.Messages) != 1 || captured.Messages[0].Role != sdk.MessageParamRoleUser {
		t.Fatalf("captured messages = %+v", captured.Messages)
	}
	if len(captured.Tools) != 1 || captured.Tools[0].OfTool == nil || captured.Tools[0].OfTool.Name != "weather" {
		t.Fatalf("captured tools = %+v", captured.Tools)
	}

	want := llm.Response{
		Provider:   "anthropic-work",
		Model:      "claude-sonnet-4-5",
		StopReason: "tool_use",
		Usage: llm.Usage{
			InputTokens:      12,
			OutputTokens:     8,
			CacheReadTokens:  1,
			CacheWriteTokens: 3,
		},
		Output: []llm.Message{
			{Role: "assistant", Parts: []llm.ContentPart{{Type: "text", Text: "Calling tool"}}},
			{Role: "assistant", ToolUse: &llm.ToolUse{Name: "weather", Arguments: `{"city":"Austin"}`, Invocation: "toolu_123"}},
		},
	}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("response = %+v, want %+v", resp, want)
	}
}

func TestProviderChatRefusalUsesExplanation(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-ant-test", nil },
		newClient: func(string) messageClient {
			return stubMessageClient{
				newFn: func(context.Context, sdk.MessageNewParams) (*sdk.Message, error) {
					return &sdk.Message{
						Model:      "claude-sonnet-4-5",
						StopReason: sdk.StopReasonRefusal,
						StopDetails: sdk.RefusalStopDetails{
							Category:    sdk.RefusalStopDetailsCategoryCyber,
							Explanation: "refused by policy",
						},
					}, nil
				},
			}
		},
	}

	resp, err := p.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "anthropic-work", Model: "claude-sonnet-4-5"})
	if err != nil {
		t.Fatalf("Chat returned err: %v", err)
	}
	if resp.Refusal != "refused by policy" {
		t.Fatalf("Refusal = %q", resp.Refusal)
	}
}

func TestProviderChatRejectsUnsupportedPart(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-ant-test", nil },
		newClient: func(string) messageClient {
			return stubMessageClient{}
		},
	}

	_, err := p.Chat(context.Background(), llm.Request{
		Operation: llm.OperationChat,
		Input: []llm.Message{{
			Role:  "user",
			Parts: []llm.ContentPart{{Type: "audio"}},
		}},
	}, llm.RouteDecision{Provider: "anthropic-work", Model: "claude-sonnet-4-5"})
	if !errors.Is(err, ErrUnsupportedInput) {
		t.Fatalf("Chat error = %v, want ErrUnsupportedInput", err)
	}
}

func TestProviderChatRequiresAPIKeyResolver(t *testing.T) {
	t.Parallel()

	p := &Provider{}
	_, err := p.Chat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "anthropic-work", Model: "claude-sonnet-4-5"})
	if !errors.Is(err, ErrAPIKeyResolverMissing) {
		t.Fatalf("Chat error = %v, want ErrAPIKeyResolverMissing", err)
	}
}

func TestProviderStreamChatEmitsTextAndFinalResponse(t *testing.T) {
	t.Parallel()

	var seen []llm.StreamEvent
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "sk-ant-test", nil },
		newClient: func(string) messageClient {
			return stubMessageClient{
				newStreamFn: func(context.Context, sdk.MessageNewParams) messageStream {
					return &stubAnthropicMessageStream{
						events: []sdk.MessageStreamEventUnion{
							mustMessageStreamEvent(t, `{"type":"message_start","message":{"id":"msg_456","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":10,"output_tokens":0}}}`),
							mustMessageStreamEvent(t, `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
							mustMessageStreamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hel"}}`),
							mustMessageStreamEvent(t, `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`),
							mustMessageStreamEvent(t, `{"type":"content_block_stop","index":0}`),
							mustMessageStreamEvent(t, `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`),
							mustMessageStreamEvent(t, `{"type":"message_stop"}`),
						},
					}
				},
			}
		},
	}

	resp, err := p.StreamChat(context.Background(), llm.Request{Operation: llm.OperationChat}, llm.RouteDecision{Provider: "anthropic-work", Model: "claude-sonnet-4-5"}, func(ev llm.StreamEvent) error {
		seen = append(seen, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat returned err: %v", err)
	}
	if len(seen) != 2 || seen[0].Delta != "hel" || seen[1].Delta != "lo" {
		t.Fatalf("seen = %+v", seen)
	}
	if resp.Model != "claude-sonnet-4-5" || len(resp.Output) != 1 || resp.Output[0].Parts[0].Text != "hello" {
		t.Fatalf("response = %+v", resp)
	}
}

type stubMessageClient struct {
	newFn       func(context.Context, sdk.MessageNewParams) (*sdk.Message, error)
	newStreamFn func(context.Context, sdk.MessageNewParams) messageStream
}

func (s stubMessageClient) New(ctx context.Context, body sdk.MessageNewParams, _ ...option.RequestOption) (*sdk.Message, error) {
	if s.newFn == nil {
		return nil, errors.New("unexpected call")
	}
	return s.newFn(ctx, body)
}

func (s stubMessageClient) NewStreaming(ctx context.Context, body sdk.MessageNewParams, _ ...option.RequestOption) messageStream {
	if s.newStreamFn == nil {
		return &stubAnthropicMessageStream{err: errors.New("unexpected call")}
	}
	return s.newStreamFn(ctx, body)
}

type stubAnthropicMessageStream struct {
	events []sdk.MessageStreamEventUnion
	err    error
	index  int
}

func (s *stubAnthropicMessageStream) Next() bool {
	if s.index >= len(s.events) {
		return false
	}
	s.index++
	return true
}

func (s *stubAnthropicMessageStream) Current() sdk.MessageStreamEventUnion {
	return s.events[s.index-1]
}

func (s *stubAnthropicMessageStream) Err() error { return s.err }

func (s *stubAnthropicMessageStream) Close() error { return nil }

func mustContentBlock(t *testing.T, raw string) sdk.ContentBlockUnion {
	t.Helper()

	var block sdk.ContentBlockUnion
	if err := json.Unmarshal([]byte(raw), &block); err != nil {
		t.Fatalf("Unmarshal(content block): %v", err)
	}
	return block
}

func mustMessageStreamEvent(t *testing.T, raw string) sdk.MessageStreamEventUnion {
	t.Helper()

	var ev sdk.MessageStreamEventUnion
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatalf("Unmarshal(message stream event): %v", err)
	}
	return ev
}
