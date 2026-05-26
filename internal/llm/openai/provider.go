package openai

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	openai "github.com/openai/openai-go"
	sdk "github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
	"github.com/openai/openai-go/packages/param"
	"github.com/openai/openai-go/responses"
	"github.com/openai/openai-go/shared"

	"github.com/hollis-labs/tether/internal/llm"
)

var (
	ErrAPIKeyResolverMissing = errors.New("openai api key resolver is nil")
	ErrUnsupportedInput      = errors.New("openai input shape not supported")
)

type apiKeyResolver func(context.Context) (string, error)

type responseClient interface {
	New(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) (*responses.Response, error)
	NewStreaming(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) responseStream
	NewEmbedding(ctx context.Context, body openai.EmbeddingNewParams, opts ...option.RequestOption) (*openai.CreateEmbeddingResponse, error)
	NewChatCompletion(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error)
	NewChatCompletionStreaming(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) chatCompletionStream
}

type clientFactory func(apiKey string) responseClient

type responseStream interface {
	Next() bool
	Current() responses.ResponseStreamEventUnion
	Err() error
	Close() error
}

type chatCompletionStream interface {
	Next() bool
	Current() openai.ChatCompletionChunk
	Err() error
	Close() error
}

type sdkResponseClient struct {
	responses       responses.ResponseService
	embeddings      openai.EmbeddingService
	chatCompletions openai.ChatCompletionService
}

func (c sdkResponseClient) New(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) (*responses.Response, error) {
	return c.responses.New(ctx, body, opts...)
}

func (c sdkResponseClient) NewStreaming(ctx context.Context, body responses.ResponseNewParams, opts ...option.RequestOption) responseStream {
	return c.responses.NewStreaming(ctx, body, opts...)
}

func (c sdkResponseClient) NewEmbedding(ctx context.Context, body openai.EmbeddingNewParams, opts ...option.RequestOption) (*openai.CreateEmbeddingResponse, error) {
	return c.embeddings.New(ctx, body, opts...)
}

func (c sdkResponseClient) NewChatCompletion(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) (*openai.ChatCompletion, error) {
	return c.chatCompletions.New(ctx, body, opts...)
}

func (c sdkResponseClient) NewChatCompletionStreaming(ctx context.Context, body openai.ChatCompletionNewParams, opts ...option.RequestOption) chatCompletionStream {
	return c.chatCompletions.NewStreaming(ctx, body, opts...)
}

// Config configures one OpenAI provider instance.
type Config struct {
	ResolveAPIKey        func(context.Context) (string, error)
	BaseURL              string
	HTTPClient           *http.Client
	AllowUnauthenticated bool
}

// Provider executes normalized llm chat requests against OpenAI's Responses API.
type Provider struct {
	resolveAPIKey apiKeyResolver
	newClient     clientFactory
	allowUnauth   bool
}

// New returns a Provider backed by the official OpenAI Go SDK.
func New(cfg Config) *Provider {
	return &Provider{
		resolveAPIKey: cfg.ResolveAPIKey,
		allowUnauth:   cfg.AllowUnauthenticated,
		newClient: func(apiKey string) responseClient {
			opts := []option.RequestOption{
				option.WithMaxRetries(0),
			}
			if apiKey != "" || !cfg.AllowUnauthenticated {
				opts = append(opts, option.WithAPIKey(apiKey))
			} else {
				// openai-go uses WithAPIKey to install auth; use a placeholder then
				// delete the header so base-URL-compatible local servers can stay unauthenticated.
				opts = append(opts, option.WithAPIKey("placeholder"), option.WithHeaderDel("authorization"))
			}
			if cfg.BaseURL != "" {
				opts = append(opts, option.WithBaseURL(cfg.BaseURL))
			}
			if cfg.HTTPClient != nil {
				opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
			}
			client := sdk.NewClient(opts...)
			return sdkResponseClient{responses: client.Responses, embeddings: client.Embeddings, chatCompletions: client.Chat.Completions}
		},
	}
}

// Chat sends one non-streaming normalized chat request to OpenAI.
func (p *Provider) Chat(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	if p.resolveAPIKey == nil && !p.allowUnauth {
		return llm.Response{}, ErrAPIKeyResolverMissing
	}
	apiKey := ""
	if p.resolveAPIKey != nil {
		var err error
		apiKey, err = p.resolveAPIKey(ctx)
		if err != nil {
			return llm.Response{}, fmt.Errorf("resolve openai api key: %w", err)
		}
	}

	params, err := buildResponseParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := p.newClient(apiKey).New(ctx, params)
	if err != nil {
		if shouldFallbackToChatCompletions(err) {
			return p.chatCompletion(ctx, req, route, apiKey)
		}
		return llm.Response{}, err
	}
	return translateResponse(resp, route), nil
}

// Embed sends one normalized embedding request to OpenAI.
func (p *Provider) Embed(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	if p.resolveAPIKey == nil && !p.allowUnauth {
		return llm.Response{}, ErrAPIKeyResolverMissing
	}
	apiKey := ""
	if p.resolveAPIKey != nil {
		var err error
		apiKey, err = p.resolveAPIKey(ctx)
		if err != nil {
			return llm.Response{}, fmt.Errorf("resolve openai api key: %w", err)
		}
	}

	params, err := buildEmbeddingParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := p.newClient(apiKey).NewEmbedding(ctx, params)
	if err != nil {
		return llm.Response{}, err
	}
	out := llm.Response{
		Provider: route.Provider,
		Model:    resp.Model,
		Usage: llm.Usage{
			InputTokens: int(resp.Usage.PromptTokens),
		},
		Embeddings: make([]llm.Embedding, 0, len(resp.Data)),
	}
	for _, item := range resp.Data {
		out.Embeddings = append(out.Embeddings, llm.Embedding{
			Index:  int(item.Index),
			Vector: append([]float64(nil), item.Embedding...),
		})
	}
	return out, nil
}

// StreamChat sends one streaming normalized chat request to OpenAI.
func (p *Provider) StreamChat(ctx context.Context, req llm.Request, route llm.RouteDecision, emit func(llm.StreamEvent) error) (llm.Response, error) {
	if p.resolveAPIKey == nil && !p.allowUnauth {
		return llm.Response{}, ErrAPIKeyResolverMissing
	}
	apiKey := ""
	if p.resolveAPIKey != nil {
		var err error
		apiKey, err = p.resolveAPIKey(ctx)
		if err != nil {
			return llm.Response{}, fmt.Errorf("resolve openai api key: %w", err)
		}
	}

	params, err := buildResponseParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	stream := p.newClient(apiKey).NewStreaming(ctx, params)
	defer stream.Close()

	var final llm.Response
	for stream.Next() {
		switch ev := stream.Current().AsAny().(type) {
		case responses.ResponseTextDeltaEvent:
			if emit != nil && strings.TrimSpace(ev.Delta) != "" {
				if err := emit(llm.StreamEvent{
					Kind:     llm.StreamEventTextDelta,
					Provider: route.Provider,
					Model:    route.Model,
					Delta:    ev.Delta,
				}); err != nil {
					return llm.Response{}, err
				}
			}
		case responses.ResponseRefusalDeltaEvent:
			if emit != nil && strings.TrimSpace(ev.Delta) != "" {
				if err := emit(llm.StreamEvent{
					Kind:     llm.StreamEventRefusalDelta,
					Provider: route.Provider,
					Model:    route.Model,
					Delta:    ev.Delta,
				}); err != nil {
					return llm.Response{}, err
				}
			}
		case responses.ResponseOutputItemDoneEvent:
			if tool, ok := ev.Item.AsAny().(responses.ResponseFunctionToolCall); ok && emit != nil {
				if err := emit(llm.StreamEvent{
					Kind:     llm.StreamEventToolUse,
					Provider: route.Provider,
					Model:    route.Model,
					ToolUse: &llm.ToolUse{
						Name:       tool.Name,
						Arguments:  tool.Arguments,
						Invocation: firstNonEmpty(tool.CallID, tool.ID),
					},
				}); err != nil {
					return llm.Response{}, err
				}
			}
		case responses.ResponseCompletedEvent:
			final = translateResponse(&ev.Response, route)
		case responses.ResponseFailedEvent:
			if ev.Response.Error.Message != "" {
				return llm.Response{}, errors.New(ev.Response.Error.Message)
			}
			return llm.Response{}, fmt.Errorf("openai response failed")
		case responses.ResponseErrorEvent:
			return llm.Response{}, fmt.Errorf("openai stream error: %s", ev.Message)
		}
	}
	if err := stream.Err(); err != nil {
		if shouldFallbackToChatCompletions(err) {
			return p.streamChatCompletion(ctx, req, route, apiKey, emit)
		}
		return llm.Response{}, err
	}
	return final, nil
}

func (p *Provider) chatCompletion(ctx context.Context, req llm.Request, route llm.RouteDecision, apiKey string) (llm.Response, error) {
	params, err := buildChatCompletionParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := p.newClient(apiKey).NewChatCompletion(ctx, params)
	if err != nil {
		return llm.Response{}, err
	}
	return translateChatCompletion(resp, route), nil
}

func (p *Provider) streamChatCompletion(ctx context.Context, req llm.Request, route llm.RouteDecision, apiKey string, emit func(llm.StreamEvent) error) (llm.Response, error) {
	params, err := buildChatCompletionParams(req, route.Model)
	if err != nil {
		return llm.Response{}, err
	}
	stream := p.newClient(apiKey).NewChatCompletionStreaming(ctx, params)
	defer stream.Close()

	final := llm.Response{Provider: route.Provider, Model: route.Model}
	var textBuilder strings.Builder
	toolCalls := map[int64]llm.ToolUse{}
	toolOrder := make([]int64, 0)
	for stream.Next() {
		chunk := stream.Current()
		if chunk.Model != "" {
			final.Model = chunk.Model
		}
		if chunk.Usage.TotalTokens > 0 || chunk.Usage.PromptTokens > 0 || chunk.Usage.CompletionTokens > 0 {
			final.Usage = llm.Usage{
				InputTokens:     int(chunk.Usage.PromptTokens),
				OutputTokens:    int(chunk.Usage.CompletionTokens),
				ReasoningTokens: int(chunk.Usage.CompletionTokensDetails.ReasoningTokens),
			}
		}
		for _, choice := range chunk.Choices {
			if choice.FinishReason != "" {
				final.StopReason = choice.FinishReason
			}
			if strings.TrimSpace(choice.Delta.Content) != "" {
				textBuilder.WriteString(choice.Delta.Content)
				if emit != nil {
					if err := emit(llm.StreamEvent{
						Kind:     llm.StreamEventTextDelta,
						Provider: route.Provider,
						Model:    final.Model,
						Delta:    choice.Delta.Content,
					}); err != nil {
						return llm.Response{}, err
					}
				}
			}
			if strings.TrimSpace(choice.Delta.Refusal) != "" {
				final.Refusal += choice.Delta.Refusal
				if emit != nil {
					if err := emit(llm.StreamEvent{
						Kind:     llm.StreamEventRefusalDelta,
						Provider: route.Provider,
						Model:    final.Model,
						Delta:    choice.Delta.Refusal,
					}); err != nil {
						return llm.Response{}, err
					}
				}
			}
			for _, tool := range choice.Delta.ToolCalls {
				existing, ok := toolCalls[tool.Index]
				if !ok {
					existing = llm.ToolUse{}
					toolOrder = append(toolOrder, tool.Index)
				}
				if existing.Invocation == "" {
					existing.Invocation = tool.ID
				}
				if existing.Name == "" {
					existing.Name = tool.Function.Name
				}
				existing.Arguments += tool.Function.Arguments
				toolCalls[tool.Index] = existing
			}
		}
	}
	if err := stream.Err(); err != nil {
		return llm.Response{}, err
	}
	if text := strings.TrimSpace(textBuilder.String()); text != "" {
		final.Output = append(final.Output, llm.Message{
			Role:  "assistant",
			Parts: []llm.ContentPart{{Type: "text", Text: text}},
		})
	}
	for _, idx := range toolOrder {
		toolUse := toolCalls[idx]
		final.Output = append(final.Output, llm.Message{
			Role:    "assistant",
			ToolUse: &toolUse,
		})
		if emit != nil {
			toolCopy := toolUse
			if err := emit(llm.StreamEvent{
				Kind:     llm.StreamEventToolUse,
				Provider: route.Provider,
				Model:    final.Model,
				ToolUse:  &toolCopy,
			}); err != nil {
				return llm.Response{}, err
			}
		}
	}
	return final, nil
}

func buildResponseParams(req llm.Request, model string) (responses.ResponseNewParams, error) {
	input, err := toInputItems(req.Input)
	if err != nil {
		return responses.ResponseNewParams{}, err
	}
	params := responses.ResponseNewParams{
		Model:           model,
		Input:           responses.ResponseNewParamsInputUnion{OfInputItemList: input},
		MaxOutputTokens: param.NewOpt(int64(maxOutputTokens(req))),
		Store:           param.NewOpt(false),
	}
	if len(req.Tools) > 0 {
		tools, err := toToolParams(req.Tools)
		if err != nil {
			return responses.ResponseNewParams{}, err
		}
		params.Tools = tools
		params.ParallelToolCalls = param.NewOpt(true)
	}
	return params, nil
}

func buildChatCompletionParams(req llm.Request, model string) (openai.ChatCompletionNewParams, error) {
	messages, err := toChatCompletionMessages(req.Input)
	if err != nil {
		return openai.ChatCompletionNewParams{}, err
	}
	params := openai.ChatCompletionNewParams{
		Model:     model,
		Messages:  messages,
		MaxTokens: param.NewOpt(int64(maxOutputTokens(req))),
		Store:     param.NewOpt(false),
	}
	if req.CallerID != "" {
		params.User = param.NewOpt(req.CallerID)
	}
	if len(req.Tools) > 0 {
		tools, err := toChatCompletionTools(req.Tools)
		if err != nil {
			return openai.ChatCompletionNewParams{}, err
		}
		params.Tools = tools
		params.ToolChoice.OfAuto = param.NewOpt("auto")
		params.ParallelToolCalls = param.NewOpt(true)
	}
	return params, nil
}

func buildEmbeddingParams(req llm.Request, model string) (openai.EmbeddingNewParams, error) {
	if len(req.EmbeddingInput) == 0 {
		return openai.EmbeddingNewParams{}, fmt.Errorf("%w: embedding_input is required", ErrUnsupportedInput)
	}
	params := openai.EmbeddingNewParams{
		Model:          model,
		Input:          openai.EmbeddingNewParamsInputUnion{OfArrayOfStrings: append([]string(nil), req.EmbeddingInput...)},
		EncodingFormat: openai.EmbeddingNewParamsEncodingFormatFloat,
	}
	if req.EmbeddingDimensions > 0 {
		params.Dimensions = param.NewOpt(int64(req.EmbeddingDimensions))
	}
	if req.CallerID != "" {
		params.User = param.NewOpt(req.CallerID)
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

func toInputItems(messages []llm.Message) (responses.ResponseInputParam, error) {
	items := make(responses.ResponseInputParam, 0, len(messages))
	for _, msg := range messages {
		item, err := toInputItem(msg)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func toInputItem(msg llm.Message) (responses.ResponseInputItemUnionParam, error) {
	if msg.ToolUse != nil {
		if strings.TrimSpace(msg.ToolUse.Name) == "" {
			return responses.ResponseInputItemUnionParam{}, fmt.Errorf("%w: tool use requires name", ErrUnsupportedInput)
		}
		return responses.ResponseInputItemUnionParam{
			OfFunctionCall: &responses.ResponseFunctionToolCallParam{
				Name:      msg.ToolUse.Name,
				Arguments: firstNonEmpty(msg.ToolUse.Arguments, "{}"),
				CallID:    firstNonEmpty(msg.ToolUse.Invocation, msg.ToolUse.Name),
			},
		}, nil
	}

	role, err := toMessageRole(msg.Role)
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	content, err := toMessageContent(msg.Parts)
	if err != nil {
		return responses.ResponseInputItemUnionParam{}, err
	}
	return responses.ResponseInputItemUnionParam{
		OfMessage: &responses.EasyInputMessageParam{
			Role:    role,
			Content: responses.EasyInputMessageContentUnionParam{OfInputItemContentList: content},
		},
	}, nil
}

func toMessageRole(role string) (responses.EasyInputMessageRole, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return responses.EasyInputMessageRoleUser, nil
	case "assistant":
		return responses.EasyInputMessageRoleAssistant, nil
	case "system":
		return responses.EasyInputMessageRoleSystem, nil
	case "developer":
		return responses.EasyInputMessageRoleDeveloper, nil
	default:
		return "", fmt.Errorf("%w: unsupported role %q", ErrUnsupportedInput, role)
	}
}

func toMessageContent(parts []llm.ContentPart) (responses.ResponseInputMessageContentListParam, error) {
	content := make(responses.ResponseInputMessageContentListParam, 0, len(parts))
	for _, part := range parts {
		item, err := toContentPart(part)
		if err != nil {
			return nil, err
		}
		content = append(content, item)
	}
	return content, nil
}

func toContentPart(part llm.ContentPart) (responses.ResponseInputContentUnionParam, error) {
	switch strings.ToLower(strings.TrimSpace(part.Type)) {
	case "text":
		return responses.ResponseInputContentUnionParam{
			OfInputText: &responses.ResponseInputTextParam{Text: part.Text},
		}, nil
	case "image":
		image, err := toImageParam(part)
		if err != nil {
			return responses.ResponseInputContentUnionParam{}, err
		}
		return responses.ResponseInputContentUnionParam{OfInputImage: &image}, nil
	default:
		return responses.ResponseInputContentUnionParam{}, fmt.Errorf("%w: unsupported part type %q", ErrUnsupportedInput, part.Type)
	}
}

func toImageParam(part llm.ContentPart) (responses.ResponseInputImageParam, error) {
	image := responses.ResponseInputImageParam{
		Detail: responses.ResponseInputImageDetailAuto,
	}
	if len(part.Data) > 0 {
		if part.MIMEType == "" {
			return responses.ResponseInputImageParam{}, fmt.Errorf("%w: image data requires mime type", ErrUnsupportedInput)
		}
		image.ImageURL = param.NewOpt("data:" + part.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(part.Data))
		return image, nil
	}
	if part.URL != "" {
		image.ImageURL = param.NewOpt(part.URL)
		return image, nil
	}
	return responses.ResponseInputImageParam{}, fmt.Errorf("%w: image part requires data or url", ErrUnsupportedInput)
}

func toToolParams(defs []llm.ToolDefinition) ([]responses.ToolUnionParam, error) {
	tools := make([]responses.ToolUnionParam, 0, len(defs))
	for _, def := range defs {
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if def.SchemaJSON != "" {
			if err := json.Unmarshal([]byte(def.SchemaJSON), &schema); err != nil {
				return nil, fmt.Errorf("parse tool schema for %q: %w", def.Name, err)
			}
		}
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        def.Name,
				Description: param.NewOpt(def.Description),
				Parameters:  schema,
				Strict:      param.NewOpt(true),
			},
		})
	}
	return tools, nil
}

func toChatCompletionMessages(messages []llm.Message) ([]openai.ChatCompletionMessageParamUnion, error) {
	out := make([]openai.ChatCompletionMessageParamUnion, 0, len(messages))
	for _, msg := range messages {
		switch strings.ToLower(strings.TrimSpace(msg.Role)) {
		case "system":
			textParts := make([]openai.ChatCompletionContentPartTextParam, 0, len(msg.Parts))
			for _, part := range msg.Parts {
				if !strings.EqualFold(part.Type, "text") {
					return nil, fmt.Errorf("%w: system messages only support text parts", ErrUnsupportedInput)
				}
				textParts = append(textParts, openai.ChatCompletionContentPartTextParam{Text: part.Text})
			}
			out = append(out, openai.SystemMessage(textParts))
		case "user":
			content, err := toChatCompletionContentParts(msg.Parts)
			if err != nil {
				return nil, err
			}
			out = append(out, openai.UserMessage(content))
		case "assistant":
			asst := openai.ChatCompletionAssistantMessageParam{}
			text := ""
			for _, part := range msg.Parts {
				if !strings.EqualFold(part.Type, "text") {
					return nil, fmt.Errorf("%w: assistant messages only support text parts", ErrUnsupportedInput)
				}
				text += part.Text
			}
			if text != "" {
				asst.Content.OfString = param.NewOpt(text)
			}
			if msg.ToolUse != nil {
				asst.ToolCalls = []openai.ChatCompletionMessageToolCallParam{{
					ID: msg.ToolUse.Invocation,
					Function: openai.ChatCompletionMessageToolCallFunctionParam{
						Name:      msg.ToolUse.Name,
						Arguments: msg.ToolUse.Arguments,
					},
				}}
			}
			out = append(out, openai.ChatCompletionMessageParamUnion{OfAssistant: &asst})
		case "tool":
			text := ""
			for _, part := range msg.Parts {
				if !strings.EqualFold(part.Type, "text") {
					return nil, fmt.Errorf("%w: tool messages only support text parts", ErrUnsupportedInput)
				}
				text += part.Text
			}
			out = append(out, openai.ToolMessage(text, msg.Name))
		default:
			return nil, fmt.Errorf("%w: unsupported role %q", ErrUnsupportedInput, msg.Role)
		}
	}
	return out, nil
}

func toChatCompletionContentParts(parts []llm.ContentPart) ([]openai.ChatCompletionContentPartUnionParam, error) {
	out := make([]openai.ChatCompletionContentPartUnionParam, 0, len(parts))
	for _, part := range parts {
		switch strings.ToLower(strings.TrimSpace(part.Type)) {
		case "text":
			out = append(out, openai.ChatCompletionContentPartUnionParam{
				OfText: &openai.ChatCompletionContentPartTextParam{Text: part.Text},
			})
		case "image":
			image, err := toChatCompletionImageParam(part)
			if err != nil {
				return nil, err
			}
			out = append(out, openai.ChatCompletionContentPartUnionParam{OfImageURL: &image})
		default:
			return nil, fmt.Errorf("%w: unsupported part type %q", ErrUnsupportedInput, part.Type)
		}
	}
	return out, nil
}

func toChatCompletionImageParam(part llm.ContentPart) (openai.ChatCompletionContentPartImageParam, error) {
	image := openai.ChatCompletionContentPartImageParam{}
	if len(part.Data) > 0 {
		if part.MIMEType == "" {
			return openai.ChatCompletionContentPartImageParam{}, fmt.Errorf("%w: image data requires mime type", ErrUnsupportedInput)
		}
		image.ImageURL.URL = "data:" + part.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(part.Data)
		return image, nil
	}
	if part.URL != "" {
		image.ImageURL.URL = part.URL
		return image, nil
	}
	return openai.ChatCompletionContentPartImageParam{}, fmt.Errorf("%w: image part requires data or url", ErrUnsupportedInput)
}

func toChatCompletionTools(defs []llm.ToolDefinition) ([]openai.ChatCompletionToolParam, error) {
	tools := make([]openai.ChatCompletionToolParam, 0, len(defs))
	for _, def := range defs {
		schema := map[string]any{"type": "object", "properties": map[string]any{}}
		if def.SchemaJSON != "" {
			if err := json.Unmarshal([]byte(def.SchemaJSON), &schema); err != nil {
				return nil, fmt.Errorf("parse tool schema for %q: %w", def.Name, err)
			}
		}
		tools = append(tools, openai.ChatCompletionToolParam{
			Function: shared.FunctionDefinitionParam{
				Name:        def.Name,
				Description: param.NewOpt(def.Description),
				Parameters:  schema,
			},
		})
	}
	return tools, nil
}

func translateResponse(resp *responses.Response, route llm.RouteDecision) llm.Response {
	out := llm.Response{
		Provider:   route.Provider,
		Model:      string(resp.Model),
		StopReason: stopReason(resp),
		Usage: llm.Usage{
			InputTokens:     int(resp.Usage.InputTokens),
			OutputTokens:    int(resp.Usage.OutputTokens),
			CacheReadTokens: int(resp.Usage.InputTokensDetails.CachedTokens),
			ReasoningTokens: int(resp.Usage.OutputTokensDetails.ReasoningTokens),
		},
	}
	for _, item := range resp.Output {
		switch variant := item.AsAny().(type) {
		case responses.ResponseOutputMessage:
			for _, content := range variant.Content {
				switch part := content.AsAny().(type) {
				case responses.ResponseOutputText:
					out.Output = append(out.Output, llm.Message{
						Role:  "assistant",
						Parts: []llm.ContentPart{{Type: "text", Text: part.Text}},
					})
				case responses.ResponseOutputRefusal:
					if out.Refusal == "" {
						out.Refusal = strings.TrimSpace(part.Refusal)
					}
				}
			}
		case responses.ResponseFunctionToolCall:
			out.Output = append(out.Output, llm.Message{
				Role: "assistant",
				ToolUse: &llm.ToolUse{
					Name:       variant.Name,
					Arguments:  variant.Arguments,
					Invocation: firstNonEmpty(variant.CallID, variant.ID),
				},
			})
		case responses.ResponseReasoningItem:
			// Keep reasoning out of message output for now; usage is preserved above.
		}
	}
	return out
}

func translateChatCompletion(resp *openai.ChatCompletion, route llm.RouteDecision) llm.Response {
	out := llm.Response{
		Provider: route.Provider,
		Model:    resp.Model,
		Usage: llm.Usage{
			InputTokens:     int(resp.Usage.PromptTokens),
			OutputTokens:    int(resp.Usage.CompletionTokens),
			ReasoningTokens: int(resp.Usage.CompletionTokensDetails.ReasoningTokens),
		},
	}
	if len(resp.Choices) == 0 {
		return out
	}
	choice := resp.Choices[0]
	out.StopReason = choice.FinishReason
	if strings.TrimSpace(choice.Message.Content) != "" {
		out.Output = append(out.Output, llm.Message{
			Role:  "assistant",
			Parts: []llm.ContentPart{{Type: "text", Text: choice.Message.Content}},
		})
	}
	if strings.TrimSpace(choice.Message.Refusal) != "" {
		out.Refusal = choice.Message.Refusal
		if out.StopReason == "" {
			out.StopReason = "refusal"
		}
	}
	for _, tool := range choice.Message.ToolCalls {
		out.Output = append(out.Output, llm.Message{
			Role: "assistant",
			ToolUse: &llm.ToolUse{
				Name:       tool.Function.Name,
				Arguments:  tool.Function.Arguments,
				Invocation: tool.ID,
			},
		})
	}
	return out
}

func shouldFallbackToChatCompletions(err error) bool {
	var apiErr *openai.Error
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func stopReason(resp *responses.Response) string {
	if resp == nil {
		return ""
	}
	if resp.Status == "incomplete" && resp.IncompleteDetails.Reason != "" {
		return resp.IncompleteDetails.Reason
	}
	for _, item := range resp.Output {
		switch item.Type {
		case "function_call":
			return "tool_use"
		case "message":
			for _, content := range item.Content {
				if content.Type == "refusal" {
					return "refusal"
				}
			}
		}
	}
	return string(resp.Status)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
