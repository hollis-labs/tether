package anthropic

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/hollis-labs/tether/internal/llm"
)

var (
	ErrAPIKeyResolverMissing = errors.New("anthropic api key resolver is nil")
	ErrUnsupportedInput      = errors.New("anthropic input shape not supported")
)

type apiKeyResolver func(context.Context) (string, error)

type messageClient interface {
	New(ctx context.Context, body sdk.MessageNewParams, opts ...option.RequestOption) (*sdk.Message, error)
	NewStreaming(ctx context.Context, body sdk.MessageNewParams, opts ...option.RequestOption) messageStream
}

type clientFactory func(apiKey string) messageClient

type messageStream interface {
	Next() bool
	Current() sdk.MessageStreamEventUnion
	Err() error
	Close() error
}

type sdkMessageClient struct {
	svc *sdk.MessageService
}

func (c sdkMessageClient) New(ctx context.Context, body sdk.MessageNewParams, opts ...option.RequestOption) (*sdk.Message, error) {
	return c.svc.New(ctx, body, opts...)
}

func (c sdkMessageClient) NewStreaming(ctx context.Context, body sdk.MessageNewParams, opts ...option.RequestOption) messageStream {
	return c.svc.NewStreaming(ctx, body, opts...)
}

// Config configures one Anthropic provider instance.
type Config struct {
	ResolveAPIKey func(context.Context) (string, error)
	BaseURL       string
	HTTPClient    *http.Client
}

// Provider executes normalized llm chat requests against Anthropic's Messages
// API.
type Provider struct {
	resolveAPIKey apiKeyResolver
	newClient     clientFactory
}

// New returns a Provider backed by the official Anthropic SDK.
func New(cfg Config) *Provider {
	return &Provider{
		resolveAPIKey: cfg.ResolveAPIKey,
		newClient: func(apiKey string) messageClient {
			opts := []option.RequestOption{
				option.WithoutEnvironmentDefaults(),
				option.WithAPIKey(apiKey),
				option.WithMaxRetries(0),
			}
			if cfg.BaseURL != "" {
				opts = append(opts, option.WithBaseURL(cfg.BaseURL))
			}
			if cfg.HTTPClient != nil {
				opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
			}
			client := sdk.NewClient(opts...)
			return sdkMessageClient{svc: &client.Messages}
		},
	}
}

// Chat sends one non-streaming normalized chat request to Anthropic.
func (p *Provider) Chat(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	if p.resolveAPIKey == nil {
		return llm.Response{}, ErrAPIKeyResolverMissing
	}
	apiKey, err := p.resolveAPIKey(ctx)
	if err != nil {
		return llm.Response{}, fmt.Errorf("resolve anthropic api key: %w", err)
	}

	params, err := buildMessageParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	msg, err := p.newClient(apiKey).New(ctx, params)
	if err != nil {
		return llm.Response{}, err
	}
	return translateResponse(msg, route), nil
}

// StreamChat sends one streaming normalized chat request to Anthropic.
func (p *Provider) StreamChat(ctx context.Context, req llm.Request, route llm.RouteDecision, emit func(llm.StreamEvent) error) (llm.Response, error) {
	if p.resolveAPIKey == nil {
		return llm.Response{}, ErrAPIKeyResolverMissing
	}
	apiKey, err := p.resolveAPIKey(ctx)
	if err != nil {
		return llm.Response{}, fmt.Errorf("resolve anthropic api key: %w", err)
	}

	params, err := buildMessageParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	stream := p.newClient(apiKey).NewStreaming(ctx, params)
	defer stream.Close()

	msg := sdk.Message{}
	for stream.Next() {
		ev := stream.Current()
		if err := msg.Accumulate(ev); err != nil {
			return llm.Response{}, err
		}
		switch variant := ev.AsAny().(type) {
		case sdk.ContentBlockDeltaEvent:
			switch delta := variant.Delta.AsAny().(type) {
			case sdk.TextDelta:
				if emit != nil && strings.TrimSpace(delta.Text) != "" {
					if err := emit(llm.StreamEvent{
						Kind:     llm.StreamEventTextDelta,
						Provider: route.Provider,
						Model:    route.Model,
						Delta:    delta.Text,
					}); err != nil {
						return llm.Response{}, err
					}
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return llm.Response{}, err
	}
	resp := translateResponse(&msg, route)
	if emit != nil {
		if resp.Refusal != "" {
			if err := emit(llm.StreamEvent{
				Kind:     llm.StreamEventRefusalDelta,
				Provider: route.Provider,
				Model:    route.Model,
				Delta:    resp.Refusal,
			}); err != nil {
				return llm.Response{}, err
			}
		}
		for _, out := range resp.Output {
			if out.ToolUse == nil {
				continue
			}
			if err := emit(llm.StreamEvent{
				Kind:     llm.StreamEventToolUse,
				Provider: route.Provider,
				Model:    route.Model,
				ToolUse:  out.ToolUse,
			}); err != nil {
				return llm.Response{}, err
			}
		}
	}
	return resp, nil
}

func buildMessageParams(req llm.Request, model string) (sdk.MessageNewParams, error) {
	params := sdk.MessageNewParams{
		Model:     model,
		MaxTokens: int64(maxOutputTokens(req)),
	}

	var system []sdk.TextBlockParam
	messages := make([]sdk.MessageParam, 0, len(req.Input))
	for _, msg := range req.Input {
		if strings.EqualFold(msg.Role, "system") {
			blocks, err := toSystemBlocks(msg)
			if err != nil {
				return sdk.MessageNewParams{}, err
			}
			system = append(system, blocks...)
			continue
		}

		translated, err := toMessageParam(msg)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		messages = append(messages, translated)
	}

	params.Messages = messages
	if len(system) > 0 {
		params.System = system
	}
	if len(req.Tools) > 0 {
		tools, err := toToolParams(req.Tools)
		if err != nil {
			return sdk.MessageNewParams{}, err
		}
		params.Tools = tools
	}
	return params, nil
}

func maxOutputTokens(req llm.Request) int {
	if req.MaxOutputTokens > 0 {
		return req.MaxOutputTokens
	}
	if req.TokenBudget > 0 {
		return req.TokenBudget
	}
	return 1024
}

func toSystemBlocks(msg llm.Message) ([]sdk.TextBlockParam, error) {
	blocks := make([]sdk.TextBlockParam, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if !strings.EqualFold(part.Type, "text") {
			return nil, fmt.Errorf("%w: system messages only support text parts", ErrUnsupportedInput)
		}
		blocks = append(blocks, sdk.TextBlockParam{Text: part.Text})
	}
	return blocks, nil
}

func toMessageParam(msg llm.Message) (sdk.MessageParam, error) {
	var role sdk.MessageParamRole
	switch strings.ToLower(strings.TrimSpace(msg.Role)) {
	case "user":
		role = sdk.MessageParamRoleUser
	case "assistant":
		role = sdk.MessageParamRoleAssistant
	default:
		return sdk.MessageParam{}, fmt.Errorf("%w: unsupported role %q", ErrUnsupportedInput, msg.Role)
	}

	blocks := make([]sdk.ContentBlockParamUnion, 0, len(msg.Parts)+1)
	for _, part := range msg.Parts {
		block, err := toContentBlock(part)
		if err != nil {
			return sdk.MessageParam{}, err
		}
		blocks = append(blocks, block)
	}
	if msg.ToolUse != nil {
		toolInput := json.RawMessage(msg.ToolUse.Arguments)
		if len(toolInput) == 0 {
			toolInput = json.RawMessage("{}")
		}
		blocks = append(blocks, sdk.ContentBlockParamUnion{
			OfToolUse: &sdk.ToolUseBlockParam{
				ID:    msg.ToolUse.Invocation,
				Input: toolInput,
				Name:  msg.ToolUse.Name,
			},
		})
	}
	return sdk.MessageParam{Role: role, Content: blocks}, nil
}

func toContentBlock(part llm.ContentPart) (sdk.ContentBlockParamUnion, error) {
	switch strings.ToLower(strings.TrimSpace(part.Type)) {
	case "text":
		return sdk.NewTextBlock(part.Text), nil
	case "image":
		if len(part.Data) > 0 {
			if part.MIMEType == "" {
				return sdk.ContentBlockParamUnion{}, fmt.Errorf("%w: image data requires mime type", ErrUnsupportedInput)
			}
			return sdk.NewImageBlockBase64(part.MIMEType, base64.StdEncoding.EncodeToString(part.Data)), nil
		}
		if part.URL != "" {
			return sdk.NewImageBlock(sdk.URLImageSourceParam{URL: part.URL}), nil
		}
		return sdk.ContentBlockParamUnion{}, fmt.Errorf("%w: image part requires data or url", ErrUnsupportedInput)
	default:
		return sdk.ContentBlockParamUnion{}, fmt.Errorf("%w: unsupported part type %q", ErrUnsupportedInput, part.Type)
	}
}

func toToolParams(defs []llm.ToolDefinition) ([]sdk.ToolUnionParam, error) {
	tools := make([]sdk.ToolUnionParam, 0, len(defs))
	for _, def := range defs {
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if def.SchemaJSON != "" {
			if err := json.Unmarshal([]byte(def.SchemaJSON), &schema); err != nil {
				return nil, fmt.Errorf("parse tool schema for %q: %w", def.Name, err)
			}
		}
		tools = append(tools, sdk.ToolUnionParam{
			OfTool: &sdk.ToolParam{
				Name:        def.Name,
				Description: sdk.String(def.Description),
				InputSchema: sdk.ToolInputSchemaParam{ExtraFields: schema},
				Type:        sdk.ToolTypeCustom,
			},
		})
	}
	return tools, nil
}

func translateResponse(msg *sdk.Message, route llm.RouteDecision) llm.Response {
	resp := llm.Response{
		Provider:   route.Provider,
		Model:      msg.Model,
		StopReason: string(msg.StopReason),
		Usage: llm.Usage{
			InputTokens:      int(msg.Usage.InputTokens),
			OutputTokens:     int(msg.Usage.OutputTokens),
			CacheReadTokens:  int(msg.Usage.CacheReadInputTokens),
			CacheWriteTokens: int(msg.Usage.CacheCreationInputTokens),
		},
	}
	if msg.StopReason == sdk.StopReasonRefusal {
		resp.Refusal = strings.TrimSpace(msg.StopDetails.Explanation)
		if resp.Refusal == "" {
			resp.Refusal = string(msg.StopDetails.Category)
		}
	}
	for _, block := range msg.Content {
		switch b := block.AsAny().(type) {
		case sdk.TextBlock:
			resp.Output = append(resp.Output, llm.Message{
				Role:  "assistant",
				Parts: []llm.ContentPart{{Type: "text", Text: b.Text}},
			})
		case sdk.ToolUseBlock:
			resp.Output = append(resp.Output, llm.Message{
				Role: "assistant",
				ToolUse: &llm.ToolUse{
					Name:       b.Name,
					Arguments:  string(b.Input),
					Invocation: b.ID,
				},
			})
		}
	}
	return resp
}
