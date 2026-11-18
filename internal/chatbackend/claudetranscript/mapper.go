package claudetranscript

import (
	"encoding/json"
)

// SignalKind is what a Signal reports.
type SignalKind int

// The signal kinds.
const (
	// TurnStart: a new turn began. Prompt holds the user's input.
	TurnStart SignalKind = iota + 1
	// TurnFrame: a frame for the open turn's claudejson decoder.
	TurnFrame
	// TurnEnd: the open turn ended. Frame is the synthetic result frame that makes the decoder emit the terminal
	// event (run.finish, or run.abort for an interrupt).
	TurnEnd
	// TurnQueued: input arrived while the turn ran and the CLI folded it into the turn. Text is the input.
	TurnQueued
)

// EndReason is why a turn ended.
type EndReason string

// The end reasons.
const (
	// EndCompleted: the transcript recorded the turn's end (stop_hook_summary or turn_duration).
	EndCompleted EndReason = "completed"
	// EndInterrupted: the transcript recorded an interrupt.
	EndInterrupted EndReason = "interrupted"
	// EndSuperseded: a new prompt started while this turn had no recorded end.
	EndSuperseded EndReason = "superseded"
	// EndForced: the caller ended the turn (End) without a transcript marker: an end it learned elsewhere (a Stop
	// hook whose marker never landed, a canceled turn the CLI did not mark, the session exiting).
	EndForced EndReason = "forced"
	// EndForcedAbort: like EndForced, but the turn is reported as aborted.
	EndForcedAbort EndReason = "forced_abort"
)

// Prompt is the input that started a turn.
type Prompt struct {
	Text   string
	Source string // promptSource: "typed", "system", ...
	Origin string // turnOrigin: "human", "system", "peer", "task_notification", ...
}

// Signal is one thing the Mapper learned from the transcript.
type Signal struct {
	Kind     SignalKind
	PromptID string // the turn's prompt id
	Prompt   Prompt // TurnStart
	Frame    []byte // TurnFrame, TurnEnd
	End      EndReason
	Text     string // TurnQueued
}

// Mapper splits a transcript into turns and yields, for each, the frames a claudejson decoder needs. Feed it the
// transcript's lines in order with Push; it is not safe for concurrent use. Lines before the first prompt it sees (a
// mapper attached mid-turn) are ignored.
type Mapper struct {
	open       string   // prompt id of the open turn
	lastClosed string   // prompt id of the turn that ended last, so its trailing lines are ignored
	curMsg     string   // id of the assistant message whose blocks are arriving
	held       [][]byte // tool result lines held until curMsg is complete
	usage      map[string]Usage
	order      []string // message ids in arrival order, for a stable usage sum
	lastStop   string
	lastTool   bool // the last assistant block was a tool call
}

// Open is the prompt id of the open turn, or "".
func (m *Mapper) Open() string { return m.open }

// Push consumes one transcript line.
func (m *Mapper) Push(raw []byte) []Signal {
	l := Parse(raw)
	var out []Signal
	switch l.Kind {
	case LineUserText:
		switch {
		case l.Interrupted():
			if m.open != "" && (l.PromptID == "" || l.PromptID == m.open) {
				out = append(out, m.end(EndInterrupted)...)
			}
		case l.PromptID != "" && l.PromptID != m.open && l.PromptID != m.lastClosed:
			if m.open != "" {
				out = append(out, m.end(EndSuperseded)...)
			}
			m.reset(l.PromptID)
			out = append(out, Signal{Kind: TurnStart, PromptID: l.PromptID, Prompt: Prompt{Text: l.Text, Source: l.Source, Origin: l.Origin}})
		}
		// Anything else is text the CLI injected into the open turn (skill bodies, reminders): not conversation.
	case LineToolResult:
		if m.open == "" {
			return nil
		}
		if m.curMsg != "" {
			m.held = append(m.held, l.Raw)
			return nil
		}
		out = append(out, m.frame(l.Raw))
	case LineAssistant:
		if m.open == "" {
			return nil
		}
		if l.MessageID != m.curMsg {
			out = m.release(out)
			m.curMsg = l.MessageID
		}
		if l.Usage != nil && l.MessageID != "" {
			if _, seen := m.usage[l.MessageID]; !seen {
				m.order = append(m.order, l.MessageID)
			}
			m.usage[l.MessageID] = *l.Usage
		}
		if l.StopReason != "" {
			m.lastStop = l.StopReason
		}
		m.lastTool = assistantIsToolUse(l.Raw)
		out = append(out, m.frame(l.Raw))
	case LineTurnEnd:
		if m.open != "" {
			out = append(out, m.end(EndCompleted)...)
		}
	case LineQueued:
		if m.open != "" && l.Text != "" {
			out = append(out, Signal{Kind: TurnQueued, PromptID: m.open, Text: l.Text})
		}
	case LineOther:
	}
	return out
}

// End ends the open turn without a transcript marker (reason EndForced or EndForcedAbort). Later lines of that turn
// are ignored. It returns nil when no turn is open.
func (m *Mapper) End(reason EndReason) []Signal {
	if m.open == "" {
		return nil
	}
	return m.end(reason)
}

func (m *Mapper) reset(prompt string) {
	m.open, m.curMsg, m.held, m.lastStop, m.lastTool = prompt, "", nil, "", false
	m.usage, m.order = map[string]Usage{}, nil
}

func (m *Mapper) frame(raw []byte) Signal {
	return Signal{Kind: TurnFrame, PromptID: m.open, Frame: raw}
}

func (m *Mapper) release(out []Signal) []Signal {
	for _, raw := range m.held {
		out = append(out, m.frame(raw))
	}
	m.held = nil
	return out
}

func (m *Mapper) end(reason EndReason) []Signal {
	out := m.release(nil)
	out = append(out, Signal{Kind: TurnEnd, PromptID: m.open, End: reason, Frame: m.resultFrame(reason)})
	m.lastClosed, m.open, m.curMsg = m.open, "", ""
	return out
}

// resultFrame is a stream-json result frame standing in for the one a transcript does not have, so the claudejson
// decoder ends the run through its own terminal path.
func (m *Mapper) resultFrame(reason EndReason) []byte {
	var sum Usage
	for _, id := range m.order {
		u := m.usage[id]
		sum.Input += u.Input
		sum.Output += u.Output
		sum.CacheCreate += u.CacheCreate
		sum.CacheRead += u.CacheRead
	}
	r := map[string]any{"type": "result", "subtype": "success", "is_error": false}
	if len(m.order) > 0 {
		r["usage"] = sum
	}
	switch reason {
	case EndCompleted:
		if m.lastStop != "" {
			r["stop_reason"] = m.lastStop
		}
	case EndInterrupted, EndForcedAbort:
		r["terminal_reason"] = "aborted_streaming"
		if m.lastTool {
			r["terminal_reason"] = "aborted_tools"
		}
	default:
		// No recorded end: say so instead of claiming a clean stop. claudejson maps an unknown terminal_reason to
		// run.finish with reason "other" and keeps this word as raw_reason.
		r["terminal_reason"] = "transcript_" + string(reason)
	}
	b, _ := json.Marshal(r)
	return b
}

func assistantIsToolUse(raw []byte) bool {
	var w struct {
		Message struct {
			Content []struct {
				Type string `json:"type"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &w) != nil {
		return false
	}
	for _, c := range w.Message.Content {
		if c.Type == "tool_use" || c.Type == "server_tool_use" || c.Type == "mcp_tool_use" {
			return true
		}
	}
	return false
}
