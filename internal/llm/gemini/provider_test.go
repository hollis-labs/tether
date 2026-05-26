package gemini

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	genai "google.golang.org/genai"
	"iter"

	"github.com/hollis-labs/tether/internal/llm"
)

func TestProviderChatBuildsRequestAndTranslatesResponse(t *testing.T) {
	t.Parallel()

	var gotModel string
	var gotContents []*genai.Content
	var gotConfig *genai.GenerateContentConfig
	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "secret", nil },
		newClient: func(_ context.Context, apiKey string) (modelClient, error) {
			if apiKey != "secret" {
				t.Fatalf("unexpected api key %q", apiKey)
			}
			return stubModelClient{
				generateContentFn: func(_ context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
					gotModel = model
					gotContents = contents
					gotConfig = config
					return &genai.GenerateContentResponse{
						ModelVersion: "gemini-2.5-flash",
						Candidates: []*genai.Candidate{{
							FinishReason: genai.FinishReasonStop,
							Content: &genai.Content{
								Parts: []*genai.Part{
									genai.NewPartFromText("hello"),
									{FunctionCall: &genai.FunctionCall{Name: "lookup", ID: "call-1", Args: map[string]any{"q": "abc"}}},
								},
							},
						}},
						UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
							PromptTokenCount:     12,
							CandidatesTokenCount: 7,
							ThoughtsTokenCount:   2,
						},
					}, nil
				},
			}, nil
		},
	}

	resp, err := p.Chat(context.Background(), llm.Request{
		Input: []llm.Message{
			{Role: "system", Parts: []llm.ContentPart{{Type: "text", Text: "be concise"}}},
			{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "what is shown?"}, {Type: "image", MIMEType: "image/png", Data: []byte("png")}}},
		},
		Tools: []llm.ToolDefinition{{
			Name:        "lookup",
			Description: "look up a value",
			SchemaJSON:  `{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}`,
		}},
		MaxOutputTokens: 256,
	}, llm.RouteDecision{Provider: "google-work", Model: "gemini-2.5-flash-preview"})
	if err != nil {
		t.Fatalf("Chat error = %v", err)
	}

	if gotModel != "gemini-2.5-flash-preview" {
		t.Fatalf("model = %q", gotModel)
	}
	if len(gotContents) != 1 || gotContents[0].Role != string(genai.RoleUser) {
		t.Fatalf("contents role mismatch: %#v", gotContents)
	}
	if len(gotContents[0].Parts) != 2 || gotContents[0].Parts[0].Text != "what is shown?" || gotContents[0].Parts[1].InlineData == nil {
		t.Fatalf("unexpected contents: %#v", gotContents[0].Parts)
	}
	if gotConfig == nil || gotConfig.SystemInstruction == nil || gotConfig.SystemInstruction.Parts[0].Text != "be concise" {
		t.Fatalf("system instruction missing: %#v", gotConfig)
	}
	if gotConfig.MaxOutputTokens != 256 {
		t.Fatalf("max output = %d", gotConfig.MaxOutputTokens)
	}
	if len(gotConfig.Tools) != 1 || len(gotConfig.Tools[0].FunctionDeclarations) != 1 {
		t.Fatalf("tools missing: %#v", gotConfig.Tools)
	}
	if gotConfig.ToolConfig == nil || gotConfig.ToolConfig.FunctionCallingConfig == nil || gotConfig.ToolConfig.FunctionCallingConfig.Mode != genai.FunctionCallingConfigModeAuto {
		t.Fatalf("tool config missing: %#v", gotConfig.ToolConfig)
	}

	if resp.Provider != "google-work" || resp.Model != "gemini-2.5-flash" || resp.StopReason != string(genai.FinishReasonStop) {
		t.Fatalf("unexpected response envelope: %#v", resp)
	}
	if len(resp.Output) != 2 || resp.Output[0].Parts[0].Text != "hello" || resp.Output[1].ToolUse == nil || resp.Output[1].ToolUse.Name != "lookup" {
		t.Fatalf("unexpected output: %#v", resp.Output)
	}
	if resp.Usage.InputTokens != 12 || resp.Usage.OutputTokens != 7 || resp.Usage.ReasoningTokens != 2 {
		t.Fatalf("unexpected usage: %#v", resp.Usage)
	}
}

func TestProviderStreamChatEmitsAndAccumulates(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "secret", nil },
		newClient: func(context.Context, string) (modelClient, error) {
			return stubModelClient{
				generateContentStreamFn: func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
					chunks := []*genai.GenerateContentResponse{
						{
							ModelVersion: "gemini-2.5-flash",
							Candidates: []*genai.Candidate{{
								Content: &genai.Content{Parts: []*genai.Part{genai.NewPartFromText("hel")}},
							}},
						},
						{
							ModelVersion: "gemini-2.5-flash",
							Candidates: []*genai.Candidate{{
								FinishReason: genai.FinishReasonStop,
								Content: &genai.Content{Parts: []*genai.Part{
									genai.NewPartFromText("lo"),
									{FunctionCall: &genai.FunctionCall{Name: "lookup", ID: "call-1", Args: map[string]any{"q": "abc"}}},
								}},
							}},
							UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
								PromptTokenCount:     10,
								CandidatesTokenCount: 5,
							},
						},
					}
					return func(yield func(*genai.GenerateContentResponse, error) bool) {
						for _, chunk := range chunks {
							if !yield(chunk, nil) {
								return
							}
						}
					}
				},
			}, nil
		},
	}

	var events []llm.StreamEvent
	resp, err := p.StreamChat(context.Background(), llm.Request{
		Input: []llm.Message{{Role: "user", Parts: []llm.ContentPart{{Type: "text", Text: "hi"}}}},
	}, llm.RouteDecision{Provider: "google-work", Model: "gemini-2.5-flash-preview"}, func(ev llm.StreamEvent) error {
		events = append(events, ev)
		return nil
	})
	if err != nil {
		t.Fatalf("StreamChat error = %v", err)
	}

	if len(events) != 3 {
		t.Fatalf("event count = %d, want 3", len(events))
	}
	if events[0].Kind != llm.StreamEventTextDelta || events[0].Delta != "hel" {
		t.Fatalf("first event = %#v", events[0])
	}
	if events[1].Kind != llm.StreamEventTextDelta || events[1].Delta != "lo" {
		t.Fatalf("second event = %#v", events[1])
	}
	if events[2].Kind != llm.StreamEventToolUse || events[2].ToolUse == nil || events[2].ToolUse.Name != "lookup" {
		t.Fatalf("third event = %#v", events[2])
	}
	if len(resp.Output) != 2 || resp.Output[0].Parts[0].Text != "hello" || resp.Output[1].ToolUse == nil {
		t.Fatalf("unexpected final output: %#v", resp.Output)
	}
	if resp.Model != "gemini-2.5-flash" || resp.StopReason != string(genai.FinishReasonStop) {
		t.Fatalf("unexpected final envelope: %#v", resp)
	}
}

func TestProviderEmbedTranslatesResponse(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "secret", nil },
		newClient: func(context.Context, string) (modelClient, error) {
			return stubModelClient{
				embedContentFn: func(_ context.Context, model string, contents []*genai.Content, _ *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
					if model != "gemini-embedding-001" {
						t.Fatalf("model = %q", model)
					}
					if len(contents) != 2 || contents[0].Parts[0].Text != "alpha" || contents[1].Parts[0].Text != "beta" {
						t.Fatalf("unexpected contents: %#v", contents)
					}
					return &genai.EmbedContentResponse{
						Embeddings: []*genai.ContentEmbedding{
							{Values: []float32{0.1, 0.2}},
							{Values: []float32{0.3}},
						},
					}, nil
				},
			}, nil
		},
	}

	resp, err := p.Embed(context.Background(), llm.Request{
		EmbeddingInput: []string{"alpha", "beta"},
	}, llm.RouteDecision{Provider: "google-work", Model: "gemini-embedding-001"})
	if err != nil {
		t.Fatalf("Embed error = %v", err)
	}
	if len(resp.Embeddings) != 2 || len(resp.Embeddings[0].Vector) != 2 || math.Abs(resp.Embeddings[1].Vector[0]-0.3) > 1e-6 {
		t.Fatalf("unexpected embeddings: %#v", resp.Embeddings)
	}
}

func TestProviderChatRejectsInvalidToolArguments(t *testing.T) {
	t.Parallel()

	p := &Provider{
		resolveAPIKey: func(context.Context) (string, error) { return "secret", nil },
		newClient: func(context.Context, string) (modelClient, error) {
			return stubModelClient{}, nil
		},
	}

	_, err := p.Chat(context.Background(), llm.Request{
		Input: []llm.Message{{
			Role:    "assistant",
			ToolUse: &llm.ToolUse{Name: "lookup", Arguments: "{not-json}"},
		}},
	}, llm.RouteDecision{Provider: "google-work", Model: "gemini-2.5-flash"})
	if err == nil || !errors.Is(err, ErrUnsupportedInput) && !strings.Contains(err.Error(), "parse tool arguments") {
		t.Fatalf("Chat error = %v, want parse failure", err)
	}
}

type stubModelClient struct {
	generateContentFn       func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error)
	generateContentStreamFn func(context.Context, string, []*genai.Content, *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error]
	embedContentFn          func(context.Context, string, []*genai.Content, *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error)
}

func (s stubModelClient) GenerateContent(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	if s.generateContentFn == nil {
		return nil, errors.New("unexpected GenerateContent call")
	}
	return s.generateContentFn(ctx, model, contents, config)
}

func (s stubModelClient) GenerateContentStream(ctx context.Context, model string, contents []*genai.Content, config *genai.GenerateContentConfig) iter.Seq2[*genai.GenerateContentResponse, error] {
	if s.generateContentStreamFn == nil {
		return func(yield func(*genai.GenerateContentResponse, error) bool) {
			yield(nil, errors.New("unexpected GenerateContentStream call"))
		}
	}
	return s.generateContentStreamFn(ctx, model, contents, config)
}

func (s stubModelClient) EmbedContent(ctx context.Context, model string, contents []*genai.Content, config *genai.EmbedContentConfig) (*genai.EmbedContentResponse, error) {
	if s.embedContentFn == nil {
		return nil, errors.New("unexpected EmbedContent call")
	}
	return s.embedContentFn(ctx, model, contents, config)
}

func TestToolUseFromFunctionCallMarshalsArgs(t *testing.T) {
	t.Parallel()

	toolUse, _, err := toolUseFromFunctionCall(&genai.FunctionCall{Name: "lookup", Args: map[string]any{"q": "abc"}})
	if err != nil {
		t.Fatalf("toolUseFromFunctionCall error = %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(toolUse.Arguments), &got); err != nil {
		t.Fatalf("toolUse args json = %v", err)
	}
	if got["q"] != "abc" {
		t.Fatalf("toolUse args = %#v", got)
	}
}
