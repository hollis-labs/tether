package llm

// OperationKind identifies the normalized AI operation the caller is
// requesting. MVP focuses on chat; embeddings and multimodal operations are
// included so the request model does not need a breaking redesign later.
type OperationKind string

const (
	OperationChat          OperationKind = "chat"
	OperationEmbedding     OperationKind = "embedding"
	OperationImageGenerate OperationKind = "image_generate"
	OperationAudioInput    OperationKind = "audio_input"
)

// Request is Tether's normalized AI gateway request. Concrete provider
// adapters translate this into vendor-specific SDK calls.
type Request struct {
	Operation OperationKind `json:"operation"`

	// Routing hints. These do not force a route unless policy chooses to honor
	// them.
	ProviderHint string `json:"provider_hint,omitempty"`
	ModelHint    string `json:"model_hint,omitempty"`
	Mode         string `json:"mode,omitempty"`
	Intent       string `json:"intent,omitempty"`

	// Correlation metadata for audit and observability.
	RequestID string `json:"request_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	CallerID  string `json:"caller_id,omitempty"`

	// Budget + execution hints.
	Streaming       bool    `json:"streaming,omitempty"`
	MaxInputTokens  int     `json:"max_input_tokens,omitempty"`
	MaxOutputTokens int     `json:"max_output_tokens,omitempty"`
	TokenBudget     int     `json:"token_budget,omitempty"`
	CostBudgetUSD   float64 `json:"cost_budget_usd,omitempty"`
	LatencyTargetMS int     `json:"latency_target_ms,omitempty"`

	Input       []Message         `json:"input,omitempty"`
	Tools       []ToolDefinition  `json:"tools,omitempty"`
	Attachments []Attachment      `json:"attachments,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// Message is a normalized conversational message. Providers that do not
// support multipart content can flatten the parts to text or reject the
// request at capability-check time.
type Message struct {
	Role    string        `json:"role"`
	Parts   []ContentPart `json:"parts,omitempty"`
	Name    string        `json:"name,omitempty"`
	ToolUse *ToolUse      `json:"tool_use,omitempty"`
}

// ContentPart is one logical content fragment within a message.
type ContentPart struct {
	Type     string `json:"type"`
	MIMEType string `json:"mime_type,omitempty"`
	Text     string `json:"text,omitempty"`
	Data     []byte `json:"data,omitempty"`
	URL      string `json:"url,omitempty"`
	Name     string `json:"name,omitempty"`
}

// ToolDefinition is the normalized shape of a callable tool schema the model
// may use during a chat request.
type ToolDefinition struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	SchemaJSON  string `json:"schema_json,omitempty"`
}

// ToolUse records a model-emitted tool call in normalized form.
type ToolUse struct {
	Name       string `json:"name"`
	Arguments  string `json:"arguments,omitempty"`
	Invocation string `json:"invocation,omitempty"`
}

// Attachment carries coarse attachment metadata for routing and capability
// checks. Provider adapters may choose to inline content or convert URLs.
type Attachment struct {
	ID       string `json:"id,omitempty"`
	Type     string `json:"type,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	Name     string `json:"name,omitempty"`
	URL      string `json:"url,omitempty"`
	Size     int64  `json:"size,omitempty"`
}

// Response is the normalized AI gateway result returned by a provider adapter
// after route planning and middleware execution.
type Response struct {
	Provider   string        `json:"provider,omitempty"`
	Model      string        `json:"model,omitempty"`
	Output     []Message     `json:"output,omitempty"`
	StopReason string        `json:"stop_reason,omitempty"`
	Refusal    string        `json:"refusal,omitempty"`
	Usage      Usage         `json:"usage,omitempty"`
	Route      RouteDecision `json:"route,omitempty"`
}

// Usage captures normalized provider usage + pricing fields.
type Usage struct {
	InputTokens      int     `json:"input_tokens,omitempty"`
	OutputTokens     int     `json:"output_tokens,omitempty"`
	CacheReadTokens  int     `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens int     `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  int     `json:"reasoning_tokens,omitempty"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd,omitempty"`
}

// RouteDecision records the chosen provider/model plus a compact explanation
// suitable for durable audit and debugging.
type RouteDecision struct {
	Provider      string   `json:"provider,omitempty"`
	Model         string   `json:"model,omitempty"`
	Reasons       []string `json:"reasons,omitempty"`
	PolicyVersion string   `json:"policy_version,omitempty"`
}

// StreamEventKind identifies one normalized incremental streaming event.
type StreamEventKind string

const (
	StreamEventStart        StreamEventKind = "response.start"
	StreamEventTextDelta    StreamEventKind = "response.output_text.delta"
	StreamEventRefusalDelta StreamEventKind = "response.refusal.delta"
	StreamEventToolUse      StreamEventKind = "response.tool_use"
	StreamEventError        StreamEventKind = "response.error"
	StreamEventCompleted    StreamEventKind = "response.completed"
)

// StreamEvent is the normalized incremental event emitted during one streaming
// AI response. The final event should be StreamEventCompleted and carry the
// accumulated Response.
type StreamEvent struct {
	Kind       StreamEventKind `json:"kind"`
	Provider   string          `json:"provider,omitempty"`
	Model      string          `json:"model,omitempty"`
	Delta      string          `json:"delta,omitempty"`
	ToolUse    *ToolUse        `json:"tool_use,omitempty"`
	Error      string          `json:"error,omitempty"`
	StopReason string          `json:"stop_reason,omitempty"`
	Usage      Usage           `json:"usage,omitempty"`
	Response   *Response       `json:"response,omitempty"`
}
