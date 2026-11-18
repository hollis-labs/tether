package claudepty

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"

	"github.com/hollis-labs/tether/internal/chatbackend/jsonltail"
)

// HookKind is the part of a Claude hook the backend acts on.
type HookKind int

// The hook kinds.
const (
	// HookSessionStarted: SessionStart (startup, resume, /clear). ClaudeSessionID names the transcript from here on.
	HookSessionStarted HookKind = iota + 1
	// HookTurnStarted: UserPromptSubmit. It fires before the prompt line reaches the transcript, and again with the same
	// prompt id when input typed mid-turn is folded into the running turn.
	HookTurnStarted
	// HookTurnCompleted: Stop. It can fire before the turn's last assistant line is in the transcript. Claude does not
	// fire it for an interrupted turn.
	HookTurnCompleted
	// HookSessionEnded: SessionEnd.
	HookSessionEnded
)

// HookSignal is one hook event, content-free (Tether's hooks record ids and sizes only).
type HookSignal struct {
	Kind            HookKind
	PromptID        string // Claude's prompt_id, the transcript's promptId
	ClaudeSessionID string
	Time            time.Time
}

// HookSource yields hook signals in the order the hooks fired until ctx ends.
type HookSource interface {
	Run(ctx context.Context, emit func(HookSignal)) error
}

// SpoolHooks follows the session's file in Tether's hook spool (agentevents.Spool: `tether hook claude` and
// `tether-worker hook claude` append one runtimeevents.Event per mapped hook). It reads the file from the start, so
// the backend learns the Claude session id and whether a turn is open even when it attaches late; events are yielded
// once each, by sequence, across the spool trimming itself.
func SpoolHooks(dir, session string) HookSource {
	return &spoolHooks{path: filepath.Join(dir, session+".jsonl")}
}

type spoolHooks struct {
	path string
	poll time.Duration
}

func (s *spoolHooks) Run(ctx context.Context, emit func(HookSignal)) error {
	var last uint64
	t := &jsonltail.Tail{Path: s.path, Poll: s.poll}
	return t.Run(ctx, func(line []byte) {
		var ev runtimeevents.Event
		if json.Unmarshal(line, &ev) != nil {
			return
		}
		if ev.Sequence != 0 {
			if ev.Sequence <= last {
				return // seen before the spool rewrote itself
			}
			last = ev.Sequence
		}
		if sig, ok := hookSignal(ev); ok {
			emit(sig)
		}
	})
}

// hookSignal maps a spooled event (agentevents.ClaudeHook's mapping) to a signal.
func hookSignal(ev runtimeevents.Event) (HookSignal, bool) {
	sig := HookSignal{PromptID: ev.TurnID, ClaudeSessionID: ev.Process.ProviderSessionID, Time: ev.Time}
	switch ev.Kind {
	case runtimeevents.KindSessionReady:
		sig.Kind = HookSessionStarted
	case runtimeevents.KindTurnStarted:
		sig.Kind = HookTurnStarted
	case runtimeevents.KindTurnCompleted:
		sig.Kind = HookTurnCompleted
	case runtimeevents.KindProcessExited:
		sig.Kind = HookSessionEnded
	default:
		return sig, false
	}
	return sig, true
}
