//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sync"
)

// Engine is a single controller's ledger. Transport and external authority
// callbacks are deliberately outside its mutex; they cannot block the reader.
type Engine struct {
	mu       sync.Mutex
	store    Store
	state    State
	limits   Limits
	poisoned bool
}

func Open(ctx context.Context, store Store, binding Binding, epoch uint64, fresh bool, limits Limits) (*Engine, error) {
	if store == nil || !binding.valid() || epoch == 0 || !limits.valid() {
		return nil, fail("invalid_config")
	}
	s, err := store.Load(ctx)
	if errors.Is(err, ErrMissing) && fresh {
		s = State{Version: Version, Binding: binding, NextID: FirstID, Epoch: epoch, Revision: 1}
		if err = store.Commit(ctx, 0, s); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	// The app records the protocol discriminator before provider placement. It
	// may bind that unused intent to the first observed journal exactly once.
	if fresh && s.Binding.Journal == "" && s.Revision != 0 && s.NextID == FirstID && s.Epoch == 0 && len(s.Operations) == 0 && len(s.Inbox) == 0 && len(s.Partial) == 0 && s.InitializeID == 0 && !s.Initialized && s.ThreadID == "" && s.ActiveTurn == "" && s.LastTerminal == "" && s.Cursor == "" && s.StreamOffset == 0 && s.PartialStart == 0 && len(s.ServerRequests) == 0 && s.ReplayHighWater == "" && s.ExitCursor == "" && s.Exit == nil {
		candidate := s.Binding
		candidate.Journal = binding.Journal
		if candidate != binding {
			return nil, fail("checkpoint_invalid")
		}
		s.Binding = binding
		previous := s.Revision
		s.Revision++
		s.Epoch = epoch
		if err := validateState(s, limits); err != nil {
			return nil, err
		}
		if err := store.Commit(ctx, previous, s); err != nil {
			return nil, err
		}
	}
	if s.Version != Version || s.Binding != binding || s.Revision == 0 || s.NextID < FirstID || s.NextID > MaxID+1 || s.Epoch > epoch || len(s.Operations) > MaxOperations || len(s.Partial) > MaxLineBytes || len(s.Inbox) > limits.InboxItems {
		return nil, fail("checkpoint_invalid")
	}
	validationState := s
	if s.Epoch == 0 {
		if !fresh || s.NextID != FirstID || len(s.Operations) != 0 || len(s.Inbox) != 0 || len(s.Partial) != 0 || len(s.ServerRequests) != 0 || s.InitializeID != 0 || s.Initialized || s.ThreadID != "" || s.ActiveTurn != "" || s.LastTerminal != "" || s.Cursor != "" || s.StreamOffset != 0 || s.ReplayHighWater != "" || s.ExitCursor != "" || s.Exit != nil {
			return nil, fail("checkpoint_invalid")
		}
		validationState.Epoch = epoch
	}
	if err := validateState(validationState, limits); err != nil {
		return nil, err
	}
	e := &Engine{store: store, state: s, limits: limits}
	if epoch != s.Epoch {
		if err = e.commitLocked(ctx, func(next *State) error { next.Epoch = epoch; next.ReplayHighWater = ""; return nil }); err != nil {
			return nil, err
		}
	}
	return e, nil
}

func clone(s State) State {
	// State has only JSON values; cloning prevents callers/store implementations
	// from mutating a committed ledger through an aliased slice or pointer.
	raw, _ := json.Marshal(s)
	var next State
	_ = json.Unmarshal(raw, &next)
	return next
}
func (e *Engine) Snapshot() State { e.mu.Lock(); defer e.mu.Unlock(); return clone(e.state) }

func (e *Engine) commitLocked(ctx context.Context, change func(*State) error) error {
	if e.poisoned {
		return fail("checkpoint_unknown")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if e.state.Revision == math.MaxUint64 {
		return fail("counter_exhausted")
	}
	next := clone(e.state)
	if err := change(&next); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	next.Revision++
	if err := validateState(next, e.limits); err != nil {
		return err
	}
	if err := e.store.Commit(ctx, e.state.Revision, next); err != nil {
		e.poisoned = true
		return fail("checkpoint_unknown")
	}
	e.state = next
	return nil
}

// Reserve persists the request identity before any write. Initialized is a
// notification but still owns a durable operation number and input intent.
func (e *Engine) Reserve(ctx context.Context, epoch uint64, method string, params json.RawMessage) (Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var op Operation
	err := e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		if !json.Valid(params) || len(params) > MaxLineBytes {
			return fail("invalid_params")
		}
		if err := admit(s, method, params); err != nil {
			return err
		}
		if s.NextID > MaxID {
			return fail("counter_exhausted")
		}
		// Forget only fully settled records. Unknown operations are never evicted
		// to admit another request; monotonic IDs prevent old-response aliasing.
		for len(s.Operations) >= MaxOperations {
			index := -1
			for i, old := range s.Operations {
				if old.Phase == Answered && !old.EffectUnknown || old.Phase == NotSubmitted {
					index = i
					break
				}
			}
			if index < 0 {
				return fail("pressure_retained")
			}
			s.Operations = append(s.Operations[:index], s.Operations[index+1:]...)
		}
		op = Operation{ID: s.NextID, Method: method, Params: append(json.RawMessage(nil), params...), Notification: method == "initialized", Phase: Intent}
		s.NextID++
		s.Operations = append(s.Operations, op)
		if method == "initialize" {
			s.InitializeID = op.ID
		}
		return nil
	})
	return op, err
}

func unresolved(s *State, method string) bool {
	for _, op := range s.Operations {
		if op.Method == method && (op.EffectUnknown || op.Phase != Answered && op.Phase != NotSubmitted) {
			return true
		}
	}
	return false
}
func admit(s *State, method string, params json.RawMessage) error {
	if method == "thread/resume" {
		return fail("native_resume_unsupported")
	}
	if err := validateParams(method, params); err != nil {
		return err
	}
	if s.Exit != nil {
		return fail("target_offline")
	}
	if method == "initialize" {
		if s.InitializeID != 0 || s.Initialized || s.ThreadID != "" {
			return fail("already_initialized_or_unknown")
		}
		return nil
	}
	if method == "initialized" {
		if s.InitializeID == 0 || s.Initialized || unresolved(s, "initialized") {
			return fail("handshake_unknown")
		}
		for _, op := range s.Operations {
			if op.ID == s.InitializeID && op.Phase == Answered && op.RPCError == nil {
				return nil
			}
		}
		return fail("handshake_unknown")
	}
	if !s.Initialized {
		return fail("not_initialized")
	}
	if method == "thread/start" {
		if s.ThreadID != "" || unresolved(s, method) {
			return fail("thread_exists_or_unknown")
		}
		for _, op := range s.Operations {
			if op.Method == "thread/start" && op.Phase != NotSubmitted {
				return fail("thread_exists_or_unknown")
			}
		}
		return nil
	}
	if method == "thread/resume" {
		return fail("native_resume_unsupported")
	}
	var p struct {
		ThreadID       string `json:"threadId"`
		TurnID         string `json:"turnId"`
		ExpectedTurnID string `json:"expectedTurnId"`
	}
	if json.Unmarshal(params, &p) != nil || s.ThreadID == "" || p.ThreadID != s.ThreadID {
		return fail("thread_mismatch")
	}
	switch method {
	case "turn/start":
		if s.ActiveTurn != "" || unresolved(s, method) {
			return fail("turn_active_or_unknown")
		}
	case "turn/steer":
		if s.ActiveTurn == "" || p.ExpectedTurnID != s.ActiveTurn {
			return fail("turn_mismatch")
		}
	case "turn/interrupt":
		if s.ActiveTurn == "" || p.TurnID != s.ActiveTurn {
			return fail("turn_mismatch")
		}
	default:
		return fail("method_unsupported")
	}
	return nil
}

func find(s *State, id uint64) *Operation {
	for i := range s.Operations {
		if s.Operations[i].ID == id {
			return &s.Operations[i]
		}
	}
	return nil
}

// Attempt must complete BEFORE calling the transport. A crash following this
// commit is uncertain even when the next process cannot observe a write.
func (e *Engine) Attempt(ctx context.Context, epoch, id uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		op := find(s, id)
		if op == nil || op.Phase != Intent {
			return fail("operation_conflict")
		}
		op.Phase = Attempted
		return nil
	})
}

func (e *Engine) BytesWritten(ctx context.Context, epoch, id uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		op := find(s, id)
		if op == nil || (op.Phase != Attempted && op.Phase != Answered) {
			return fail("operation_conflict")
		}
		// A fast response can precede the shim's bytes receipt.
		if op.Phase != Answered {
			op.Phase = Written
		}
		if op.Notification {
			op.Phase = Answered
			if op.Method == "initialized" {
				s.Initialized = true
			}
			if op.Method == "$server_reply" {
				for i := range s.ServerRequests {
					if s.ServerRequests[i].InputID == id {
						s.ServerRequests[i].Written = true
					}
				}
			}
		}
		return nil
	})
}

// ClaimServerRequest precedes the authority hook. A crashed/uncertain callback is retained,
// never invoked again just because the controller reconnects.
func (e *Engine) ClaimServerRequest(ctx context.Context, source string) (ServerRequest, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var request ServerRequest
	err := e.commitLocked(ctx, func(s *State) error {
		if !s.Initialized || s.Exit != nil {
			return fail("not_initialized")
		}
		for i := range s.ServerRequests {
			if s.ServerRequests[i].Source != source {
				continue
			}
			if s.ServerRequests[i].Claimed {
				return fail("server_callback_unknown")
			}
			s.ServerRequests[i].Claimed = true
			request = s.ServerRequests[i]
			return nil
		}
		return fail("server_request_conflict")
	})
	return request, err
}

func (e *Engine) ReserveServerReply(ctx context.Context, epoch uint64, source string, wire json.RawMessage) (Operation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	var op Operation
	err := e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch || s.Exit != nil {
			return fail("epoch_mismatch")
		}
		if !json.Valid(wire) || len(wire) > 64<<10 {
			return fail("invalid_params")
		}
		if s.NextID > MaxID {
			return fail("counter_exhausted")
		}
		for i := range s.ServerRequests {
			r := &s.ServerRequests[i]
			if r.Source != source {
				continue
			}
			if !r.Claimed || r.InputID != 0 || r.Written {
				return fail("server_callback_unknown")
			}
			var response struct {
				ID json.RawMessage `json:"id"`
			}
			if json.Unmarshal(wire, &response) != nil || string(response.ID) != string(r.ID) {
				return fail("server_request_conflict")
			}
			if len(s.Operations) >= MaxOperations {
				return fail("pressure_retained")
			}
			op = Operation{ID: s.NextID, Method: "$server_reply", Params: append(json.RawMessage(nil), wire...), Notification: true, Phase: Intent}
			s.NextID++
			r.InputID = op.ID
			s.Operations = append(s.Operations, op)
			return nil
		}
		return fail("server_request_conflict")
	})
	return op, err
}

// AbandonUnsubmitted is allowed only while the durable record proves no submit
// was attempted. After Attempt no observer error can erase the obligation.
func (e *Engine) AbandonUnsubmitted(ctx context.Context, epoch, id uint64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		if epoch != s.Epoch {
			return fail("epoch_mismatch")
		}
		op := find(s, id)
		if op == nil || op.Phase != Intent {
			return fail("outcome_unknown")
		}
		op.Phase = NotSubmitted
		if op.Method == "initialize" {
			s.InitializeID = 0
		}
		return nil
	})
}

func (e *Engine) AcceptReplayHighWater(ctx context.Context, epoch uint64, cursor string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.commitLocked(ctx, func(s *State) error {
		if s.Epoch != epoch {
			return fail("epoch_mismatch")
		}
		if _, err := cursorNumber(s.Binding.Journal, cursor); err != nil {
			return err
		}
		s.ReplayHighWater = cursor
		return nil
	})
}
