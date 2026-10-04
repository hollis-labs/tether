//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/substrate/harness/shim"
)

// Transport injects complete provider frames; Accepted means bytes_written,
// not remote RPC success. It never retries. The implementation must serialize
// writes, correlate inject receipts and bound/interrupt every wait on ctx.
type Transport interface {
	Validate(context.Context) error
	Inject(context.Context, string, []byte) error
	Replay(context.Context, string) error
	Ack(context.Context, string) error
	Close() error
}

type Session struct {
	engine          *Engine
	transport       Transport
	ctx             context.Context
	cancel          context.CancelFunc
	writeGate       chan struct{}
	mu              sync.Mutex
	changed         chan struct{}
	done            chan struct{}
	once            sync.Once
	err             error
	exitCode        int
	providerPID     int
	highWater       string
	onSessionID     func(string)
	workers         sync.WaitGroup
	deliveryChecker DeliveryChecker
}

func NewSession(engine *Engine, transport Transport, providerPID int) (*Session, error) {
	if engine == nil || transport == nil || providerPID <= 0 {
		return nil, fail("identity_mismatch")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Session{engine: engine, transport: transport, ctx: ctx, cancel: cancel, changed: make(chan struct{}), done: make(chan struct{}), providerPID: providerPID, writeGate: make(chan struct{}, 1)}, nil
}

func (s *Session) signal() {
	s.mu.Lock()
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}
func (s *Session) changedSince() <-chan struct{} { s.mu.Lock(); defer s.mu.Unlock(); return s.changed }

func bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

// Call cancellation ends the observer, not a possibly submitted remote call.
// Its durable operation remains resolvable by the sole reader after cancellation.
func (s *Session) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	ctx, cancel := bounded(ctx)
	defer cancel()
	op, err := s.submit(ctx, method, params)
	if err != nil {
		return nil, err
	}
	if op.Notification {
		return json.RawMessage(`{}`), nil
	}
	for {
		changed := s.changedSince()
		state := s.engine.Snapshot()
		if result := find(&state, op.ID); result != nil && result.Phase == Answered {
			if result.RPCError != nil {
				if result.RPCError.Code == -32601 {
					return nil, fail("capability_unsupported")
				}
				return nil, &agentsessions.JsonRpcError{Code: result.RPCError.Code, Message: result.RPCError.Message, Data: result.RPCError.Data}
			}
			return append(json.RawMessage(nil), result.Result...), nil
		}
		select {
		case <-ctx.Done():
			return nil, fail("outcome_unknown")
		case <-s.done:
			return nil, fail("outcome_unknown")
		case <-changed:
		}
	}
}

func (s *Session) submit(ctx context.Context, method string, params any) (Operation, error) {
	select {
	case s.writeGate <- struct{}{}:
	case <-ctx.Done():
		return Operation{}, ctx.Err()
	case <-s.done:
		return Operation{}, fail("detached")
	}
	defer func() { <-s.writeGate }()
	select {
	case <-s.done:
		return Operation{}, fail("detached")
	default:
	}
	if err := ctx.Err(); err != nil {
		return Operation{}, err
	}
	if err := s.transport.Validate(ctx); err != nil {
		return Operation{}, err
	}
	select {
	case <-s.done:
		return Operation{}, fail("detached")
	default:
	}
	raw, err := json.Marshal(params)
	if err != nil {
		return Operation{}, fail("invalid_params")
	}
	// Refuse an oversized single inject BEFORE recording a possible effect.
	if len(raw) > shim.OutputChunk-1024 {
		return Operation{}, fail("frame_too_large")
	}
	epoch := s.engine.Snapshot().Epoch
	op, err := s.engine.Reserve(ctx, epoch, method, raw)
	if err != nil {
		return Operation{}, err
	}
	frame := struct {
		ID     uint64          `json:"id,omitempty"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}{ID: op.ID, Method: method, Params: raw}
	if op.Notification {
		frame.ID = 0
	}
	data, err := json.Marshal(frame)
	if err != nil {
		return op, fail("invalid_params")
	}
	validationErr := s.transport.Validate(ctx)
	if validationErr == nil {
		validationErr = ctx.Err()
	}
	select {
	case <-s.done:
		validationErr = fail("detached")
	default:
	}
	if validationErr != nil {
		post, stop := bounded(context.Background())
		defer stop()
		if abandonErr := s.engine.AbandonUnsubmitted(post, epoch, op.ID); abandonErr != nil {
			return op, abandonErr
		}
		return op, validationErr
	}
	if err = s.engine.Attempt(ctx, epoch, op.ID); err != nil {
		return op, err
	}
	if err = s.transport.Inject(ctx, "codex-"+strconv.FormatUint(op.ID, 10), append(data, '\n')); err != nil {
		// No retry or false NoChange classification from a local transport error.
		return op, fail("outcome_unknown")
	}
	post, stop := bounded(context.Background())
	defer stop()
	if err = s.engine.BytesWritten(post, epoch, op.ID); err != nil {
		return op, err
	}
	s.signal()
	return op, nil
}

// AcceptOutput is called only by the sole shim reader. State and private
// inbox are durable before ACK; public output delivery is still an obligation.
func (s *Session) AcceptOutput(ctx context.Context, cursor, stream string, data []byte) error {
	_, err := s.engine.AcceptOutput(ctx, s.engine.Snapshot().Epoch, cursor, stream, data)
	if err != nil {
		return err
	}
	if err = s.transport.Ack(ctx, s.engine.Snapshot().Cursor); err != nil {
		return fail("ack_unknown")
	}
	s.signal()
	return nil
}

func (s *Session) Initialize(ctx context.Context, clientName, version string) error {
	state := s.engine.Snapshot()
	if state.Initialized {
		return nil
	}
	if state.InitializeID != 0 {
		return fail("handshake_unknown")
	}
	if _, err := s.Call(ctx, "initialize", map[string]any{"clientInfo": map[string]string{"name": clientName, "version": version}}); err != nil {
		return err
	}
	_, err := s.Call(ctx, "initialized", map[string]any{})
	return err
}

func (s *Session) SendTurn(ctx context.Context, text, cwd, clientName, version string) error {
	if err := s.Initialize(ctx, clientName, version); err != nil {
		return err
	}
	state := s.engine.Snapshot()
	if state.ThreadID == "" {
		params := map[string]any{}
		if cwd != "" {
			params["cwd"] = cwd
		}
		if _, err := s.Call(ctx, "thread/start", params); err != nil {
			return err
		}
		state = s.engine.Snapshot()
	}
	_, err := s.Call(ctx, "turn/start", map[string]any{"threadId": state.ThreadID, "input": []map[string]string{{"type": "text", "text": text}}})
	return err
}

func (s *Session) Steer(ctx context.Context, expected, text string) error {
	state := s.engine.Snapshot()
	if expected == "" || state.ActiveTurn != expected {
		return fail("turn_mismatch")
	}
	_, err := s.Call(ctx, "turn/steer", map[string]any{"threadId": state.ThreadID, "expectedTurnId": expected, "input": []map[string]string{{"type": "text", "text": text}}})
	return err
}
func (s *Session) InterruptTurn(ctx context.Context) error {
	state := s.engine.Snapshot()
	if state.ActiveTurn == "" {
		if unresolved(&state, "turn/start") {
			return fail("outcome_unknown")
		}
		return nil
	}
	_, err := s.Call(ctx, "turn/interrupt", map[string]string{"threadId": state.ThreadID, "turnId": state.ActiveTurn})
	return err
}

// Finish is controller-local unless passed a journaled provider exit by the
// reader. No transport EOF or Stop kills a provider or synthesizes its exit.
func (s *Session) Finish(code int, err error) {
	s.once.Do(func() {
		s.mu.Lock()
		s.exitCode, s.err = code, err
		s.mu.Unlock()
		s.cancel()
		_ = s.transport.Close()
		close(s.done)
		s.signal()
	})
}
func (s *Session) Wait() (int, error) {
	<-s.done
	s.workers.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.exitCode, s.err
}
func (s *Session) Stop(context.Context) error                            { s.Finish(0, fail("detached")); return nil }
func (s *Session) SendInput(context.Context, []byte) error               { return fail("raw_input_unsupported") }
func (s *Session) Resize(context.Context, uint16, uint16) error          { return fail("resize_unsupported") }
func (s *Session) CheckpointHints() (agentsessions.CheckpointHint, bool) { return nil, false }
func (s *Session) ProviderSessionID() string                             { return s.engine.Snapshot().ThreadID }
func (s *Session) Health() agentsessions.HealthStatus {
	state := s.engine.Snapshot()
	h := agentsessions.HealthStatus{Alive: true, PID: s.providerPID, State: agentsessions.LiveStateIdle, TurnID: state.ActiveTurn}
	if state.ActiveTurn != "" || unresolved(&state, "turn/start") {
		h.State = agentsessions.LiveStateProcessing
	}
	select {
	case <-s.done:
		h.Alive = false
		h.State = agentsessions.LiveStateStopped
	default:
	}
	return h
}
func (s *Session) Readiness() error {
	state := s.engine.Snapshot()
	s.mu.Lock()
	high := s.highWater
	s.mu.Unlock()
	if high == "" || state.ReplayHighWater != high {
		return fail("replay_pending")
	}
	position, err := cursorNumber(state.Binding.Journal, state.Cursor)
	if state.Cursor == "" {
		position, err = 0, nil
	}
	target, targetErr := cursorNumber(state.Binding.Journal, high)
	if err != nil || targetErr != nil || position < target {
		return fail("replay_pending")
	}
	if !state.Initialized {
		return fail("handshake_unknown")
	}
	if state.Exit != nil {
		return fail("target_offline")
	}
	for _, op := range state.Operations {
		if op.EffectUnknown || op.Phase == Attempted || op.Phase == Written || op.Phase == Intent {
			return fail("input_pending")
		}
	}
	for _, request := range state.ServerRequests {
		if !request.Written {
			return fail("server_callback_unknown")
		}
	}
	if len(state.Inbox) != 0 || len(state.Partial) != 0 {
		return fail("output_pending")
	}
	// No successful B2 delivery issuer is installed in this source slice.
	if s.deliveryChecker == nil {
		return fail("output_pending")
	}
	work, stop := bounded(s.ctx)
	defer stop()
	if err := s.deliveryChecker.CheckDelivery(work, state, high); err != nil {
		return fail("output_pending")
	}
	if work.Err() != nil || s.engine.Snapshot().Revision != state.Revision {
		return fail("output_pending")
	}
	s.mu.Lock()
	unchanged := s.highWater == high && s.ctx.Err() == nil
	s.mu.Unlock()
	if !unchanged {
		return fail("output_pending")
	}
	return nil
}

// WaitReadiness bounds replay settling; every other typed disposition remains
// non-running. It never initializes a provider or drains B2 output implicitly.
func (s *Session) WaitReadiness(ctx context.Context) error {
	for {
		changed := s.changedSince()
		err := s.Readiness()
		if !HasCode(err, "replay_pending") {
			return err
		}
		select {
		case <-changed:
		case <-s.done:
			return fail("detached")
		case <-ctx.Done():
			return fail("replay_pending")
		}
	}
}

// The hook is the existing narrow Tether handler, not a general background
// plugin executor. Reader/ledger locks are not held while it executes. Its
// durable claim prevents reconnect from repeating an uncertain grant callback.
func (s *Session) serverRequests(hook func(string, json.RawMessage) (any, *agentsessions.JsonRpcError)) {
	for {
		changed := s.changedSince()
		state := s.engine.Snapshot()
		processed := false
		for _, request := range state.ServerRequests {
			if request.Claimed {
				continue
			}
			work, stop := bounded(s.ctx)
			err := s.transport.Validate(work)
			var claimed ServerRequest
			if err == nil {
				claimed, err = s.engine.ClaimServerRequest(work, request.Source)
			}
			var result any
			var rpcErr *agentsessions.JsonRpcError
			if err == nil {
				if hook == nil {
					rpcErr = &agentsessions.JsonRpcError{Code: -32601, Message: "server request unsupported"}
				} else {
					result, rpcErr = hook(claimed.Method, append(json.RawMessage(nil), claimed.Params...))
				}
				err = s.transport.Validate(work)
			}
			if err == nil {
				err = s.replyServer(work, claimed, result, rpcErr)
			}
			stop()
			if err != nil {
				s.Finish(0, err)
				return
			}
			processed = true
		}
		if processed {
			continue
		}
		select {
		case <-s.ctx.Done():
			return
		case <-changed:
		}
	}
}

func (s *Session) replyServer(ctx context.Context, request ServerRequest, result any, rpcErr *agentsessions.JsonRpcError) error {
	select {
	case s.writeGate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return fail("detached")
	}
	defer func() { <-s.writeGate }()
	if err := s.transport.Validate(ctx); err != nil {
		return err
	}
	select {
	case <-s.done:
		return fail("detached")
	default:
	}
	response := map[string]any{"id": request.ID}
	if rpcErr != nil {
		response["error"] = map[string]any{"code": rpcErr.Code, "message": rpcErr.Message, "data": rpcErr.Data}
	} else {
		response["result"] = result
	}
	wire, err := json.Marshal(response)
	if err != nil || len(wire)+1 > shim.OutputChunk {
		return fail("invalid_params")
	}
	epoch := s.engine.Snapshot().Epoch
	op, err := s.engine.ReserveServerReply(ctx, epoch, request.Source, wire)
	if err != nil {
		return err
	}
	if err = s.transport.Validate(ctx); err != nil {
		return err
	}
	select {
	case <-s.done:
		return fail("detached")
	default:
	}
	if err = s.engine.Attempt(ctx, epoch, op.ID); err != nil {
		return err
	}
	if err = s.transport.Inject(ctx, "codex-"+strconv.FormatUint(op.ID, 10), append(wire, '\n')); err != nil {
		return fail("outcome_unknown")
	}
	post, stop := bounded(context.Background())
	defer stop()
	if err := s.engine.BytesWritten(post, epoch, op.ID); err != nil {
		return err
	}
	s.signal()
	return nil
}

func (s *Session) setHighWater(cursor string) {
	if _, err := cursorNumber(s.engine.Snapshot().Binding.Journal, cursor); err != nil {
		s.Finish(0, err)
		return
	}
	work, stop := bounded(s.ctx)
	err := s.engine.AcceptReplayHighWater(work, s.engine.Snapshot().Epoch, cursor)
	stop()
	if err != nil {
		s.Finish(0, err)
		return
	}
	s.mu.Lock()
	s.highWater = cursor
	s.mu.Unlock()
	s.signal()
}
func (s *Session) String() string {
	return fmt.Sprintf("hosted Codex session %s", s.engine.Snapshot().Binding.Session)
}

var _ agentsessions.Session = (*Session)(nil)
var _ agentsessions.JsonRpcCaller = (*Session)(nil)
var _ agentsessions.TurnInterrupter = (*Session)(nil)

// DeliveryChecker is installed only by the app's private verified-store
// consumer. No RPC caller or provider payload supplies this port.
type DeliveryChecker interface {
	CheckDelivery(context.Context, State, string) error
}
