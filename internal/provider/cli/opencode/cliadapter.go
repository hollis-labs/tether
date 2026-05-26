package opencode

import (
	"encoding/json"
	"errors"
	"os/exec"

	"github.com/hollis-labs/agentkit/agentsessions"
	llmtypes "github.com/hollis-labs/go-llm-types"
	gop "github.com/hollis-labs/go-providers/provider"
	events "github.com/hollis-labs/go-providers/provider/events"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/provider/cli/claudestream"
)

// New constructs an agentsessions.Runtime that drives the opencode CLI's
// `run --format json` mode. Each session's per-turn subprocess is spawned
// by go-runner; the top-level "sessionID" field on every JSON event line
// is captured for `--session <id>` continuity across turns.
//
// opencode has no go-providers CLIAdapter, so we ship a minimal one here
// (cliAdapter, below). It cooperates with claudestream's PlanScopedAdapter
// to thread plan.Command + plan.Args through the catalog-resolved binary.
func New(plan *launch.Plan) (agentsessions.Runtime, error) {
	return claudestream.NewWithAdapter(plan, &cliAdapter{}, "opencode", agentsessions.Capabilities{
		PTY:               false,
		Resize:            false,
		ProviderSessionID: true,
		CheckpointResume:  false,
		BinaryRequired:    true,
	})
}

// cliAdapter implements gop.CLIAdapter for the opencode CLI. PlanScopedAdapter
// prepends plan.Args (catalog convention "run") before the per-turn argv built
// here, so this adapter only owns the per-turn flag shape.
type cliAdapter struct{}

func (cliAdapter) Name() string { return "opencode" }

func (cliAdapter) Clone() gop.CLIAdapter { return &cliAdapter{} }

// BuildArgs constructs the per-turn argv: --format json, optional --session
// <id> when resuming, then the prompt. systemPrompt is unused (opencode
// reads its system context from the ambient project state, not a flag).
func (cliAdapter) BuildArgs(prompt, _systemPrompt, cliSessionID string) []string {
	out := []string{"--format", "json"}
	if cliSessionID != "" {
		out = append(out, "--session", cliSessionID)
	}
	out = append(out, prompt)
	return out
}

// cliEventEnvelope captures the only field cliAdapter looks at from each
// opencode JSON event line: the top-level sessionID. Everything else is
// passed through verbatim as an EventDelta so the attach broker sees the
// raw NDJSON stream the TUI parses today. Named to avoid collision with
// the legacy adapter's eventEnvelope; both die together in commit 2.
type cliEventEnvelope struct {
	SessionID string `json:"sessionID"`
}

// ParseLine emits an EventSessionID when the line carries a non-empty
// sessionID, plus an EventDelta carrying the raw line so subscribers see
// the wire format unchanged. Lines that don't parse as JSON are still
// forwarded as deltas — opencode emits the occasional non-JSON
// diagnostic and dropping it would lose information.
func (cliAdapter) ParseLine(line []byte) ([]llmtypes.StreamEvent, error) {
	if len(line) == 0 {
		return nil, nil
	}
	out := []llmtypes.StreamEvent{
		{Type: llmtypes.EventDelta, Content: string(line) + "\n"},
	}
	var ev cliEventEnvelope
	if err := json.Unmarshal(line, &ev); err == nil && ev.SessionID != "" {
		out = append(out, llmtypes.StreamEvent{Type: llmtypes.EventSessionID, SessionID: ev.SessionID})
	}
	return out, nil
}

// Detect resolves "opencode" on PATH. PlanScopedAdapter overrides this
// when plan.Command is set, which is the common case (catalog declares
// the binary path explicitly), so this fallback only fires on bare
// invocations / tests.
func (cliAdapter) Detect() (string, bool) {
	path, err := exec.LookPath("opencode")
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return "", false
		}
		return "", false
	}
	return path, true
}

func (cliAdapter) BootDirSpec() gop.BootDirSpec {
	return gop.NewOpencodeAdapter().BootDirSpec()
}

func (cliAdapter) ParseLineEvents(line []byte) ([]events.Event, error) {
	if p, ok := any(gop.NewOpencodeAdapter()).(gop.EventParser); ok {
		return p.ParseLineEvents(line)
	}
	return nil, nil
}
