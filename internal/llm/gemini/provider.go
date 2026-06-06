package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	genai "google.golang.org/genai"
	"iter"

	"github.com/hollis-labs/tether/internal/llm"
)

var (
	ErrAPIKeyResolverMissing = errors.New("gemini api key resolver is nil")
	ErrUnsupportedInput      = errors.New("gemini input shape not supported")
)

type apiKeyResolver func(context.Context) (string, error)

type modelClient interface {
	GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
	GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
	EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error)
}

type clientFactory func(context.Context, string) (modelClient, error)

type sdkModelClient struct {
	models *genai.Models
}

func (c sdkModelClient) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	return c.models.GenerateContent(ctx, model, contents, config)
}

func (c sdkModelClient) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	return c.models.GenerateContentStream(ctx, model, contents, config)
}

func (c sdkModelClient) EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
	return c.models.EmbedContent(ctx, model, contents, config)
}

type Config struct {
	ResolveAPIKey func(context.Context) (string, error)
	BaseURL       string
	HTTPClient    *http.Client
}

type Provider struct {
	resolveAPIKey apiKeyResolver
	newClient     clientFactory
}

func New(cfg Config) *Provider {
	return &Provider{
		resolveAPIKey: cfg.ResolveAPIKey,
		newClient: func(ctx context.Context, apiKey string) (modelClient, error) {
			clientCfg := &genai.ClientConfig{
				APIKey:     apiKey,
				Backend:    genai.BackendGeminiAPI,
				HTTPClient: cfg.HTTPClient,
			}
			if cfg.BaseURL != "" {
				clientCfg.HTTPOptions.BaseURL = cfg.BaseURL
			}
			client, err := genai.NewClient(ctx, clientCfg)
			if err != nil {
				return nil, err
			}
			return sdkModelClient{models: client.Models}, nil
		},
	}
}

func (p *Provider) Chat(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	client, err := p.client(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	contents, cfg, err := buildGenerateContentRequest(req)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := client.GenerateContent(ctx, route.Model, contents, cfg)
	if err != nil {
		return llm.Response{}, err
	}
	return translateResponse(resp, route), nil
}

func (p *Provider) StreamChat(ctx context.Context, req llm.Request, route llm.RouteDecision, emit func(llm.StreamEvent) error) (llm.Response, error) {
	client, err := p.client(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	contents, cfg, err := buildGenerateContentRequest(req)
	if err != nil {
		return llm.Response{}, err
	}

	final := llm.Response{
		Provider: route.Provider,
		Model:    route.Model,
	}
	var textBuilder strings.Builder
	var toolUses []llm.ToolUse
	seenTools := map[string]struct{}{}
	refusalEmitted := false

	for chunk, err := range client.GenerateContentStream(ctx, route.Model, contents, cfg) {
		if err != nil {
			return llm.Response{}, err
		}
		if chunk == nil {
			continue
		}
		if chunk.ModelVersion != "" {
			final.Model = chunk.ModelVersion
		}
		applyUsage(&final, chunk.UsageMetadata)

		if refusal := promptFeedbackRefusal(chunk.PromptFeedback); refusal != "" {
			final.Refusal = refusal
			final.StopReason = "refusal"
			if emit != nil && !refusalEmitted {
				refusalEmitted = true
				if err := emit(llm.StreamEvent{
					Kind:     llm.StreamEventRefusalDelta,
					Provider: route.Provider,
					Model:    final.Model,
					Delta:    refusal,
				}); err != nil {
					return llm.Response{}, err
				}
			}
		}

		for _, candidate := range chunk.Candidates {
			if candidate.FinishReason != "" {
				final.StopReason = string(candidate.FinishReason)
			}
			if candidate.Content == nil {
				continue
			}
			for _, part := range candidate.Content.Parts {
				if part == nil {
					continue
				}
				if part.Text != "" && !part.Thought {
					textBuilder.WriteString(part.Text)
					if emit != nil && strings.TrimSpace(part.Text) != "" {
						if err := emit(llm.StreamEvent{
							Kind:     llm.StreamEventTextDelta,
							Provider: route.Provider,
							Model:    final.Model,
							Delta:    part.Text,
						}); err != nil {
							return llm.Response{}, err
						}
					}
				}
				if part.FunctionCall == nil {
					continue
				}
				toolUse, signature, err := toolUseFromFunctionCall(part.FunctionCall)
				if err != nil {
					return llm.Response{}, err
				}
				if _, ok := seenTools[signature]; ok {
					continue
				}
				seenTools[signature] = struct{}{}
				toolUses = append(toolUses, toolUse)
				if emit != nil {
					toolUseCopy := toolUse
					if err := emit(llm.StreamEvent{
						Kind:     llm.StreamEventToolUse,
						Provider: route.Provider,
						Model:    final.Model,
						ToolUse:  &toolUseCopy,
					}); err != nil {
						return llm.Response{}, err
					}
				}
			}
		}
	}

	if text := strings.TrimSpace(textBuilder.String()); text != "" {
		final.Output = append(final.Output, llm.Message{
			Role:  "assistant",
			Parts: []llm.ContentPart{{Type: "text", Text: text}},
		})
	}
	for _, toolUse := range toolUses {
		toolUseCopy := toolUse
		final.Output = append(final.Output, llm.Message{
			Role:    "assistant",
			ToolUse: &toolUseCopy,
		})
	}
	return final, nil
}

func (p *Provider) Embed(ctx context.Context, req llm.Request, route llm.RouteDecision) (llm.Response, error) {
	client, err := p.client(ctx)
	if err != nil {
		return llm.Response{}, err
	}
	contents, cfg, err := buildEmbedRequest(req)
	if err != nil {
		return llm.Response{}, err
	}
	resp, err := client.EmbedContent(ctx, route.Model, contents, cfg)
	if err != nil {
		return llm.Response{}, err
	}
	out := llm.Response{
		Provider:   route.Provider,
		Model:      route.Model,
		Embeddings: make([]llm.Embedding, 0, len(resp.Embeddings)),
	}
	for i, item := range resp.Embeddings {
		if item == nil {
			continue
		}
		vector := make([]float64, 0, len(item.Values))
		for _, value := range item.Values {
			vector = append(vector, float64(value))
		}
		out.Embeddings = append(out.Embeddings, llm.Embedding{
			Index:  i,
			Vector: vector,
		})
	}
	return out, nil
}

func (p *Provider) client(ctx context.Context) (modelClient, error) {
	if p.resolveAPIKey == nil {
		return nil, ErrAPIKeyResolverMissing
	}
	apiKey, err := p.resolveAPIKey(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve gemini api key: %w", err)
	}
	client, err := p.newClient(ctx, apiKey)
	if err != nil {
		return nil, fmt.Errorf("create gemini client: %w", err)
	}
	return client, nil
}

func buildGenerateContentRequest(req llm.Request) ([]*genai.Content, *genai.GenerateContentConfig, error) {
	mot := maxOutputTokens(req)
	if mot < 0 {
		mot = 0
	}
	if mot > math.MaxInt32 {
		mot = math.MaxInt32
	}
	cfg := &genai.GenerateContentConfig{
		MaxOutputTokens: int32(mot),
	}
	var systemParts []*genai.Part
	contents := make([]*genai.Content, 0, len(req.Input))
	for _, msg := range req.Input {
		if strings.EqualFold(msg.Role, "system") {
			parts, err := toSystemParts(msg)
			if err != nil {
				return nil, nil, err
			}
			systemParts = append(systemParts, parts...)
			continue
		}

		content, err := toContent(msg)
		if err != nil {
			return nil, nil, err
		}
		contents = append(contents, content)
	}
	if len(systemParts) > 0 {
		cfg.SystemInstruction = genai.NewContentFromParts(systemParts, genai.RoleUser)
	}
	if len(req.Tools) > 0 {
		tools, err := toTools(req.Tools)
		if err != nil {
			return nil, nil, err
		}
		cfg.Tools = tools
		cfg.ToolConfig = &genai.ToolConfig{
			FunctionCallingConfig: &genai.FunctionCallingConfig{
				Mode: genai.FunctionCallingConfigModeAuto,
			},
		}
	}
	return contents, cfg, nil
}

func buildEmbedRequest(req llm.Request) ([]*genai.Content, *genai.EmbedContentConfig, error) {
	if req.EmbeddingDimensions > 0 {
		return nil, nil, fmt.Errorf("%w: embedding_dimensions is not supported by gemini", ErrUnsupportedInput)
	}
	contents := make([]*genai.Content, 0, len(req.EmbeddingInput))
	for _, text := range req.EmbeddingInput {
		if strings.TrimSpace(text) == "" {
			continue
		}
		contents = append(contents, genai.NewContentFromText(text, genai.RoleUser))
	}
	if len(contents) == 0 {
		return nil, nil, fmt.Errorf("%w: embedding_input is required", ErrUnsupportedInput)
	}
	return contents, &genai.EmbedContentConfig{}, nil
}

func toSystemParts(msg llm.Message) ([]*genai.Part, error) {
	parts := make([]*genai.Part, 0, len(msg.Parts))
	for _, part := range msg.Parts {
		if !strings.EqualFold(part.Type, "text") {
			return nil, fmt.Errorf("%w: system messages only support text parts", ErrUnsupportedInput)
		}
		parts = append(parts, genai.NewPartFromText(part.Text))
	}
	return parts, nil
}

func toContent(msg llm.Message) (*genai.Content, error) {
	role, err := toRole(msg.Role)
	if err != nil {
		return nil, err
	}
	parts := make([]*genai.Part, 0, len(msg.Parts)+1)
	for _, part := range msg.Parts {
		translated, err := toPart(part)
		if err != nil {
			return nil, err
		}
		parts = append(parts, translated)
	}
	if msg.ToolUse != nil {
		args, err := parseArguments(msg.ToolUse.Arguments)
		if err != nil {
			return nil, fmt.Errorf("parse tool arguments for %q: %w", msg.ToolUse.Name, err)
		}
		functionCall := genai.NewPartFromFunctionCall(msg.ToolUse.Name, args)
		if msg.ToolUse.Invocation != "" {
			functionCall.FunctionCall.ID = msg.ToolUse.Invocation
		}
		parts = append(parts, functionCall)
	}
	return genai.NewContentFromParts(parts, role), nil
}

func toRole(role string) (genai.Role, error) {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "user":
		return genai.RoleUser, nil
	case "assistant":
		return genai.RoleModel, nil
	default:
		return "", fmt.Errorf("%w: unsupported role %q", ErrUnsupportedInput, role)
	}
}

func toPart(part llm.ContentPart) (*genai.Part, error) {
	switch strings.ToLower(strings.TrimSpace(part.Type)) {
	case "text":
		return genai.NewPartFromText(part.Text), nil
	case "image":
		if part.MIMEType == "" {
			return nil, fmt.Errorf("%w: image part requires mime type", ErrUnsupportedInput)
		}
		if len(part.Data) > 0 {
			return genai.NewPartFromBytes(part.Data, part.MIMEType), nil
		}
		if part.URL != "" {
			return genai.NewPartFromURI(part.URL, part.MIMEType), nil
		}
		return nil, fmt.Errorf("%w: image part requires data or url", ErrUnsupportedInput)
	default:
		return nil, fmt.Errorf("%w: unsupported part type %q", ErrUnsupportedInput, part.Type)
	}
}

func toTools(defs []llm.ToolDefinition) ([]*genai.Tool, error) {
	declarations := make([]*genai.FunctionDeclaration, 0, len(defs))
	for _, def := range defs {
		schema := any(nil)
		if def.SchemaJSON != "" {
			if err := json.Unmarshal([]byte(def.SchemaJSON), &schema); err != nil {
				return nil, fmt.Errorf("parse tool schema for %q: %w", def.Name, err)
			}
		}
		declarations = append(declarations, &genai.FunctionDeclaration{
			Name:                 def.Name,
			Description:          def.Description,
			ParametersJsonSchema: schema,
		})
	}
	return []*genai.Tool{{FunctionDeclarations: declarations}}, nil
}

func parseArguments(raw string) (map[string]any, error) {
	if strings.TrimSpace(raw) == "" {
		return map[string]any{}, nil
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

func translateResponse(resp *genai.GenerateContentResponse, route llm.RouteDecision) llm.Response {
	out := llm.Response{
		Provider: route.Provider,
		Model:    route.Model,
	}
	if resp == nil {
		return out
	}
	if resp.ModelVersion != "" {
		out.Model = resp.ModelVersion
	}
	applyUsage(&out, resp.UsageMetadata)
	if refusal := promptFeedbackRefusal(resp.PromptFeedback); refusal != "" {
		out.Refusal = refusal
		out.StopReason = "refusal"
	}
	for _, candidate := range resp.Candidates {
		if candidate == nil {
			continue
		}
		if candidate.FinishReason != "" {
			out.StopReason = string(candidate.FinishReason)
		}
		if candidate.Content == nil {
			continue
		}
		for _, part := range candidate.Content.Parts {
			if part == nil {
				continue
			}
			if part.Text != "" && !part.Thought {
				out.Output = append(out.Output, llm.Message{
					Role:  "assistant",
					Parts: []llm.ContentPart{{Type: "text", Text: part.Text}},
				})
			}
			if part.FunctionCall != nil {
				toolUse, _, err := toolUseFromFunctionCall(part.FunctionCall)
				if err != nil {
					continue
				}
				toolUseCopy := toolUse
				out.Output = append(out.Output, llm.Message{
					Role:    "assistant",
					ToolUse: &toolUseCopy,
				})
			}
		}
	}
	return out
}

func toolUseFromFunctionCall(call *genai.FunctionCall) (llm.ToolUse, string, error) {
	arguments, err := json.Marshal(call.Args)
	if err != nil {
		return llm.ToolUse{}, "", fmt.Errorf("marshal function call args: %w", err)
	}
	toolUse := llm.ToolUse{
		Name:       call.Name,
		Arguments:  string(arguments),
		Invocation: call.ID,
	}
	signature := call.ID + ":" + call.Name + ":" + toolUse.Arguments
	return toolUse, signature, nil
}

func promptFeedbackRefusal(feedback *genai.GenerateContentResponsePromptFeedback) string {
	if feedback == nil || feedback.BlockReason == "" {
		return ""
	}
	return strings.TrimSpace(string(feedback.BlockReason))
}

func applyUsage(out *llm.Response, usage *genai.GenerateContentResponseUsageMetadata) {
	if usage == nil {
		return
	}
	out.Usage = llm.Usage{
		InputTokens:     int(usage.PromptTokenCount),
		OutputTokens:    int(usage.CandidatesTokenCount),
		CacheReadTokens: int(usage.CachedContentTokenCount),
		ReasoningTokens: int(usage.ThoughtsTokenCount),
	}
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
