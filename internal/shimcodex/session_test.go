//go:build !windows

package shimcodex

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"sync"
	"testing"
	"time"
)

type fixtureTransport struct {
	mu                    sync.Mutex
	validate              func(context.Context) error
	inject                func(context.Context, string, []byte) error
	injects, acks, closes int
}

func (t *fixtureTransport) Validate(ctx context.Context) error {
	if t.validate != nil {
		return t.validate(ctx)
	}
	return ctx.Err()
}
func (t *fixtureTransport) Inject(ctx context.Context, key string, data []byte) error {
	t.mu.Lock()
	t.injects++
	t.mu.Unlock()
	if t.inject != nil {
		return t.inject(ctx, key, data)
	}
	return nil
}
func (*fixtureTransport) Replay(context.Context, string) error { return nil }
func (t *fixtureTransport) Ack(context.Context, string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.acks++
	return nil
}
func (t *fixtureTransport) Close() error { t.mu.Lock(); defer t.mu.Unlock(); t.closes++; return nil }

func TestSessionValidatesBeforeIntentAndAfterCallback(t *testing.T) {
	for _, where := range []string{"before", "after", "cancel_after", "detach_after"} {
		t.Run(where, func(t *testing.T) {
			e, _ := newEngine(t)
			transport := &fixtureTransport{}
			s, err := NewSession(e, transport, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop(context.Background()) })
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			transport.validate = func(context.Context) error {
				calls++
				if where == "before" {
					return errors.New("revoked")
				}
				if calls == 2 {
					switch where {
					case "after":
						return errors.New("revoked late")
					case "cancel_after":
						cancel()
					case "detach_after":
						_ = s.Stop(context.Background())
					}
				}
				return nil
			}
			if _, err = s.Call(ctx, "initialize", map[string]any{}); err == nil {
				t.Fatal("revoked call succeeded")
			}
			if transport.injects != 0 {
				t.Fatal("revoked call wrote provider bytes")
			}
			state := e.Snapshot()
			if where == "before" {
				if len(state.Operations) != 0 || state.NextID != FirstID {
					t.Fatal("prevalidation persisted input")
				}
			} else if len(state.Operations) != 1 || state.Operations[0].Phase != NotSubmitted || state.InitializeID != 0 {
				t.Fatalf("known no submit erased/misclassified: %+v", state)
			}
		})
	}
}

func TestSessionCancelAfterSubmitRetainsAndNeverResends(t *testing.T) {
	e, m := newEngine(t)
	boundThread(t, e)
	transport := &fixtureTransport{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	transport.inject = func(context.Context, string, []byte) error { cancel(); return errors.New("reply lost") }
	s, err := NewSession(e, transport, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	if _, err = s.Call(ctx, "turn/start", map[string]any{"threadId": "native-t"}); !HasCode(err, "outcome_unknown") {
		t.Fatal(err)
	}
	if transport.injects != 1 {
		t.Fatal("unexpected submit count")
	}
	next, err := Open(context.Background(), m, e.Snapshot().Binding, 2, false, e.limits)
	if err != nil {
		t.Fatal(err)
	}
	replacement := &fixtureTransport{}
	reattached, err := NewSession(next, replacement, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reattached.Stop(context.Background()) })
	if err = reattached.SendTurn(context.Background(), "do not retry", "", "fixture", "1"); !HasCode(err, "turn_active_or_unknown") {
		t.Fatal(err)
	}
	if replacement.injects != 0 {
		t.Fatal("uncertain turn was resent on reattach")
	}
}

func TestSessionDurableAcceptancePrecedesAckAndStopIsDetach(t *testing.T) {
	e, m := newEngine(t)
	transport := &fixtureTransport{}
	s, err := NewSession(e, transport, 1)
	if err != nil {
		t.Fatal(err)
	}
	m.commitError = errors.New("durability failed")
	if err = s.AcceptOutput(context.Background(), "j:1", "stdout", []byte("{\"method\":\"item/agentMessage/delta\"}\n")); !HasCode(err, "checkpoint_unknown") {
		t.Fatal(err)
	}
	if transport.acks != 0 {
		t.Fatal("ACK preceded durable inbox")
	}
	_ = s.Stop(context.Background())
	_ = s.Stop(context.Background())
	if _, err = s.Wait(); !HasCode(err, "detached") {
		t.Fatal(err)
	}
	if transport.closes != 1 || e.Snapshot().Exit != nil {
		t.Fatal("detach fabricated provider exit or double-closed")
	}
}

func TestServerCallbackClaimAndReplySurviveCrashBoundaries(t *testing.T) {
	for _, boundary := range []string{"before_callback", "after_callback_revoke", "stale_epoch", "reply_uncertain", "reply_written"} {
		t.Run(boundary, func(t *testing.T) {
			e, m := newEngine(t)
			boundThread(t, e)
			receive(t, e, 3, `{"id":7,"method":"mcpServer/elicitation/request","params":{"fixture":true}}`)
			source := e.Snapshot().ServerRequests[0].Source
			if boundary == "before_callback" {
				if _, err := e.ClaimServerRequest(context.Background(), source); err != nil {
					t.Fatal(err)
				}
			} else {
				transport := &fixtureTransport{}
				revoked := false
				transport.validate = func(ctx context.Context) error {
					if revoked {
						return errors.New("revoked after callback")
					}
					return ctx.Err()
				}
				if boundary == "reply_uncertain" {
					transport.inject = func(context.Context, string, []byte) error { return errors.New("receipt lost after possible reply") }
				}
				s, err := NewSession(e, transport, 1)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = s.Stop(context.Background()); _, _ = s.Wait() })
				s.workers.Add(1)
				go func() {
					defer s.workers.Done()
					s.serverRequests(func(string, json.RawMessage) (any, *agentsessions.JsonRpcError) {
						if !e.Snapshot().ServerRequests[0].Claimed {
							t.Error("authority callback preceded durable claim")
						}
						switch boundary {
						case "after_callback_revoke":
							revoked = true
						case "stale_epoch":
							if _, err := Open(context.Background(), m, e.Snapshot().Binding, 2, false, e.limits); err != nil {
								t.Error(err)
							}
						}
						return map[string]string{"decision": "accept"}, nil
					})
				}()
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if boundary == "reply_written" {
					waitProtocol(ctx, t, s, func(state State) bool { return state.ServerRequests[0].Written })
					_ = s.Stop(ctx)
				}
				select {
				case <-s.done:
				case <-ctx.Done():
					t.Fatal("callback/reply boundary did not settle")
				}
				_, _ = s.Wait()
				expected := 0
				if boundary == "reply_uncertain" || boundary == "reply_written" {
					expected = 1
				}
				if transport.injects != expected {
					t.Fatalf("late authority/epoch submitted reply: %d", transport.injects)
				}
			}
			previous, err := m.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			next, err := Open(context.Background(), m, previous.Binding, previous.Epoch+1, false, e.limits)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = next.ClaimServerRequest(context.Background(), source); !HasCode(err, "server_callback_unknown") {
				t.Fatalf("reconnect repeated authority callback: %v", err)
			}
			if next.Snapshot().ServerRequests[0].Written != (boundary == "reply_written") {
				t.Fatal("uncertain reply promoted or committed reply lost")
			}
		})
	}
}

func waitProtocol(ctx context.Context, t *testing.T, s *Session, predicate func(State) bool) State {
	t.Helper()
	for {
		changed := s.changedSince()
		state := s.engine.Snapshot()
		if predicate(state) {
			return state
		}
		select {
		case <-changed:
		case <-s.done:
			t.Fatalf("controller ended: %v", s.err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestMetadataDurabilityAndHighwaterNeverFabricateOutput(t *testing.T) {
	e, m := newEngine(t)
	if err := e.AcceptMetadata(context.Background(), 1, "j:1", "shim.attached", json.RawMessage(`{"epoch":"1"}`)); err != nil {
		t.Fatal(err)
	}
	s, err := NewSession(e, &fixtureTransport{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Stop(context.Background()) })
	s.setHighWater("j:1")
	if !HasCode(s.Readiness(), "handshake_unknown") {
		t.Fatal("metadata fabricated handshake readiness")
	}
	state := e.Snapshot()
	if state.Exit != nil || state.InitializeID != 0 || state.ThreadID != "" || len(state.Inbox) != 1 || state.Cursor != "j:1" {
		t.Fatal("metadata fabricated protocol/output settlement")
	}
	if err = e.AcceptMetadata(context.Background(), 1, "j:1", "shim.attached", json.RawMessage(`{"epoch":"1"}`)); err != nil {
		t.Fatal(err)
	}
	if len(e.Snapshot().Inbox) != 1 {
		t.Fatal("metadata replay duplicated source identity")
	}
	m.commitError = errors.New("durability lost")
	if err = e.AcceptMetadata(context.Background(), 1, "j:2", "shim.detached", json.RawMessage(`{}`)); !HasCode(err, "checkpoint_unknown") {
		t.Fatal(err)
	}
	if e.Snapshot().Cursor != "j:1" || len(e.Snapshot().Inbox) != 1 {
		t.Fatal("failed metadata commit advanced cursor")
	}
}

type fixtureDeliveryChecker func(context.Context, State, string) error

func (f fixtureDeliveryChecker) CheckDelivery(ctx context.Context, state State, high string) error {
	return f(ctx, state, high)
}

func TestSessionReadinessNeverUsesEmptyInboxOrStaleDeliveryObservation(t *testing.T) {
	for _, kind := range []string{"absent", "unsupported", "revision_after_load", "detach_after_load"} {
		t.Run(kind, func(t *testing.T) {
			e, _ := newEngine(t)
			handshake(t, e)
			// Private consumer model only: no actual B2 issuer or durable drain.
			e.mu.Lock()
			err := e.commitLocked(context.Background(), func(state *State) error { state.Inbox = nil; return nil })
			e.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			s, err := NewSession(e, &fixtureTransport{}, 1)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Stop(context.Background()) })
			s.setHighWater("j:1")
			if kind != "absent" {
				s.deliveryChecker = fixtureDeliveryChecker(func(ctx context.Context, state State, high string) error {
					switch kind {
					case "unsupported":
						return errors.New("unavailable")
					case "revision_after_load":
						return e.AcceptReplayHighWater(ctx, state.Epoch, high)
					case "detach_after_load":
						return s.Stop(ctx)
					}
					return nil
				})
			}
			if err := s.Readiness(); !HasCode(err, "output_pending") {
				t.Fatalf("unearned readiness: %v", err)
			}
		})
	}
}
