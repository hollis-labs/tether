// Package claudestream parses the newline-delimited JSON event stream
// emitted by the `claude` CLI when invoked with
// `--output-format stream-json --verbose`.
//
// # Event model
//
// The CLI emits events of a handful of types: system init (carrying the
// session_id used for --resume), assistant messages (text + tool_use
// content blocks), rate-limit notices, final result (with usage
// breakdown and stop_reason), and top-level errors. Each line of
// stdout is one JSON object.
//
// Parse translates a single line into zero or more logical Events.
// Unknown or uninteresting types (rate_limit_event, system/non-init)
// return zero events and nil error — they're informational noise the
// library filters.
//
// # Extractable package
//
// This package is intentionally placed outside internal/ so it can be
// promoted to `~/Projects-apps/framework/libs/go-claudestream` (or a
// standalone repo) once agent-mux validates the shape. A second
// consumer (Nanite) will migrate to the promoted package and retire
// its own copy. See README.md for the promotion plan.
//
// # Source lineage
//
// Parser logic was copied from Nanite's provider package
// (pkg/provider/pty_claude.go) with the permission of the same author.
// Event-type naming was reshaped (Kind string constants instead of
// bare string literals) but the wire protocol handling is unchanged.
package claudestream

import "encoding/json"

// Kind names the logical event variant. Values match Nanite's
// string conventions so a future swap between the two packages is a
// rename, not a semantic change.
type Kind string

const (
	// KindSessionID carries the CLI session_id emitted in a
	// system/init event. Consumers persist this to enable
	// `claude --resume <session_id>` on a subsequent turn.
	KindSessionID Kind = "session_id"

	// KindDelta is an assistant-message text delta. Multiple
	// KindDelta events form the assistant's visible output.
	KindDelta Kind = "delta"

	// KindToolUse is an assistant-message tool_use block. The
	// associated ToolUse holds the tool name + input.
	KindToolUse Kind = "tool_use"

	// KindUsage is the final token-usage report for a CLI run.
	KindUsage Kind = "usage"

	// KindDone closes a CLI run. Emitted after KindUsage.
	KindDone Kind = "done"

	// KindError reports either a top-level transport error
	// ("error" envelope) or a run that ended with is_error=true.
	KindError Kind = "error"
)

// Event is the uniform return shape. Which fields are populated
// depends on Kind — see the per-Kind doc above. Unused fields are
// zero-valued.
type Event struct {
	Kind      Kind
	Text      string        // populated for KindDelta
	ToolUse   *ToolUseBlock // populated for KindToolUse
	Usage     *Usage        // populated for KindUsage
	ErrorMsg  string        // populated for KindError
	SessionID string        // populated for KindSessionID
}

// ToolUseBlock is the content of a claude tool_use block: the unique
// block ID, the tool name (Read, Write, Bash, …), and the tool
// input as a parsed-from-JSON map.
type ToolUseBlock struct {
	ID    string
	Name  string
	Input map[string]any
}

// Usage mirrors claude's usage report, flattened into per-run totals.
// Token counts are pre-summed across model-specific breakdowns; the
// raw model_usage field from the wire protocol is ignored.
type Usage struct {
	InputTokens         int
	OutputTokens        int
	CacheCreationTokens int
	CacheReadTokens     int
	StopReason          string
}

// ---------------------------------------------------------------------
// Wire-format structs. Unexported; only the typed Event above leaks to
// callers.
// ---------------------------------------------------------------------

type wireEnvelope struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`
}

type wireAssistantEvent struct {
	Type    string        `json:"type"`
	Message wireAssistant `json:"message"`
}

type wireAssistant struct {
	Role    string             `json:"role"`
	Content []wireContentBlock `json:"content"`
	Usage   *wireUsage         `json:"usage,omitempty"`
}

type wireContentBlock struct {
	Type  string          `json:"type"`
	Text  string          `json:"text,omitempty"`
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type wireResultEvent struct {
	Type       string     `json:"type"`
	Subtype    string     `json:"subtype"`
	IsError    bool       `json:"is_error"`
	Result     string     `json:"result"`
	StopReason string     `json:"stop_reason"`
	Usage      *wireUsage `json:"usage,omitempty"`
}

type wireSystemEvent struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
}

type wireErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
}
