//go:build !windows

// Package shimcodex owns the protocol state of a Codex app-server whose stdio
// transport lives in a shim. A new controller is not a new provider transport.
package shimcodex

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/harness/shim"
)

const (
	Version              = "codex-appserver-v1"
	FirstID       uint64 = 1 << 32
	MaxID         uint64 = 1<<53 - 1 // JSON numeric IDs must remain exact for JS peers.
	MaxOperations        = 64
	MaxLineBytes         = 64 << 20
)

type Failure struct{ Code string }

func (e *Failure) ErrorCode() string { return e.Code }
func (e *Failure) Error() string     { return "hosted Codex: " + e.Code }
func fail(code string) error         { return &Failure{Code: code} }
func HasCode(err error, code string) bool {
	var f *Failure
	return errors.As(err, &f) && f.Code == code
}

// Binding comes from the canonical placement, not caller-supplied home paths.
// Epoch changes under the same binding; Journal identifies the provider pipes.
type Binding struct {
	Session     string `json:"session"`
	Instance    string `json:"instance"`
	Generation  uint64 `json:"generation,string"`
	Operation   string `json:"placement_operation"`
	Attempt     string `json:"submission_attempt"`
	Fingerprint string `json:"fingerprint"`
	Journal     string `json:"journal"`
}

func (b Binding) valid() bool {
	return b.Session != "" && b.Instance != "" && b.Generation != 0 && b.Operation != "" && b.Journal != "" && b.Attempt != "" && b.Fingerprint != ""
}

type Phase string

const (
	Intent       Phase = "intent"
	Attempted    Phase = "attempted"
	Written      Phase = "bytes_written"
	Answered     Phase = "answered"
	NotSubmitted Phase = "not_submitted"
)

// Operation keeps unknown calls even after their observer cancels. A bytes
// receipt never proves RPC success, and an unanswered operation is not retried.
type Operation struct {
	ID            uint64          `json:"id,string"`
	Method        string          `json:"method"`
	Params        json.RawMessage `json:"params"`
	Notification  bool            `json:"notification,omitempty"`
	Phase         Phase           `json:"phase"`
	Result        json.RawMessage `json:"result,omitempty"`
	RPCError      *RPCError       `json:"error,omitempty"`
	EffectUnknown bool            `json:"effect_unknown,omitempty"`
}
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

type Event struct {
	Identity string          `json:"identity"`
	Cursor   string          `json:"cursor"`
	Raw      json.RawMessage `json:"raw"`
}

type ServerRequest struct {
	Source  string          `json:"source"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Claimed bool            `json:"claimed"`
	InputID uint64          `json:"input_id,string"`
	Written bool            `json:"reply_written"`
}

// State and Inbox are committed together. Cursor means private durable
// acceptance, not delivery to a UI, approval callback or output reducer.
type State struct {
	Version         string          `json:"version"`
	Binding         Binding         `json:"binding"`
	Revision        uint64          `json:"revision,string"`
	Epoch           uint64          `json:"epoch,string"`
	NextID          uint64          `json:"next_id,string"`
	InitializeID    uint64          `json:"initialize_id,string"`
	Initialized     bool            `json:"initialized"`
	ThreadID        string          `json:"thread_id,omitempty"`
	ActiveTurn      string          `json:"active_turn,omitempty"`
	LastTerminal    string          `json:"last_terminal,omitempty"`
	Operations      []Operation     `json:"operations"`
	ReplayHighWater string          `json:"replay_high_water"`
	ExitCursor      string          `json:"exit_cursor,omitempty"`
	Cursor          string          `json:"cursor"`
	StreamOffset    uint64          `json:"stream_offset,string"`
	Partial         []byte          `json:"partial,omitempty"`
	PartialStart    uint64          `json:"partial_start,string"`
	Inbox           []Event         `json:"inbox"`
	Exit            *shim.Exit      `json:"exit,omitempty"`
	ServerRequests  []ServerRequest `json:"server_requests,omitempty"`
}

// Store.Commit is one conditional durable transaction over the entire state.
// Error (including an ambiguous commit) poisons the live Engine: reconnect
// must reload the canonical store before any more input can be admitted.
type Store interface {
	Load(context.Context) (State, error)
	Commit(context.Context, uint64, State) error
}

var ErrMissing = errors.New("hosted Codex checkpoint absent")

type Limits struct {
	InboxItems int
	InboxBytes int
}

func (l Limits) valid() bool { return l.InboxItems > 0 && l.InboxBytes > 0 }
