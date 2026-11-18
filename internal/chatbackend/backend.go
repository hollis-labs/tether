// Package chatbackend is the seam between a Tether session and the chat wire (chatstream/v1): one Backend per running
// agent session submits a user turn, cancels it, yields the turn's events, and reports turns the agent ran that were not
// submitted through it (observed runs). Task CW-20261118-0010; design in direction/proposals/chat-attach-wire.md.
//
// Two implementations, one per Claude runtime:
//
//   - claudepty: the Claude Code TUI in a PTY. Content comes from the vendor transcript (whole messages, no deltas), turn
//     boundaries from Tether's Claude hooks, submit and cancel are keystrokes.
//   - claudestdio: `claude -p --input-format stream-json --output-format stream-json`. Content and deltas come from
//     stdout through chatstream/adapter/claudejson; submit is a stream-json user frame, cancel an interrupt request.
//
// The interface is shaped so a chat.Runner (libs/agent/chat; internal/chatwire.Runner) wraps a Backend unchanged:
// Runner.Start is Submit, Run.Events and Run.Cancel are the same, Info.Deltas is the runner's capabilities.deltas, and
// Observed feeds the turns the Service did not start. Approvals are not bridged: agents keep their own permission mode,
// and a Backend reports none (Info.Approvals false).
package chatbackend

import (
	"context"
	"errors"
	"iter"

	"github.com/hollis-labs/libs/ui/ui-go/chatstream"
)

// Backend runs chat turns against one agent session.
type Backend interface {
	// Info reports what this backend's runs carry.
	Info() Info

	// Submit delivers one user turn to the agent and returns its run once the input was handed to the agent. The run's
	// events start with run.start and end with exactly one terminal event. A backend that cannot accept a turn now (the
	// agent is in the middle of another one) returns ErrBusy and delivers nothing.
	Submit(ctx context.Context, turn Turn) (Run, error)

	// Observed yields a run for every turn the agent started that did not come from Submit: text typed into the terminal,
	// an injected mail wake, a plugin message. Each carries the user's text, because no one else saw it. The channel is
	// closed when the backend stops.
	Observed() <-chan ObservedRun
}

// Turn is one user turn to submit.
type Turn struct {
	// RunID names the run. A backend generates one when it is empty.
	RunID string
	// Text is the user's input, delivered as one message.
	Text string
}

// Run is one turn's event stream.
type Run interface {
	// ID is the run_id every event of the run carries.
	ID() string
	// Events yields the run's chatstream events in order and ends after exactly one terminal event (run.finish, run.error
	// or run.abort). Seq is left zero. A run has one consumer; a second call yields nothing.
	Events() iter.Seq2[chatstream.Event, error]
	// Cancel asks the turn to stop. The run then ends with run.abort. Canceling an ended run does nothing.
	Cancel()
}

// ObservedRun is a run the backend saw start without a Submit.
type ObservedRun struct {
	Run
	// Text is the user's input as the agent received it.
	Text string
	// Source is the vendor's word for how the input arrived (Claude: "typed", "system"), Origin who it came from
	// (Claude: "human", "system", "peer", "task_notification"). Either may be empty.
	Source string
	Origin string
	// TurnRef is the vendor's own id for the turn (Claude's prompt id), for joining with hook events.
	TurnRef string
}

// Info is what a backend's runs carry.
type Info struct {
	// Deltas is false for a backend whose runs carry whole parts only (no part.delta).
	Deltas bool
	// Approvals is false: no backend bridges the agent's permission prompts yet.
	Approvals bool
}

// ErrBusy is Submit's answer when the agent is in the middle of a turn. A TUI would queue the input and fold it into the
// running turn, where it would never become a run of its own, so it is refused instead.
var ErrBusy = errors.New("chatbackend: agent is busy with another turn")

// ErrClosed is Submit's answer once the backend has stopped.
var ErrClosed = errors.New("chatbackend: backend is closed")
