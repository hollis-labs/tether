package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/hollis-labs/agent-contracts-leaf/runtimes"
	"github.com/hollis-labs/agentkit/agentruntime/turn"
	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
)

const nativeResumeGrace = 2 * time.Second

// recoveryRuntime owns only the initial recovery attempt. Returning the raw
// session preserves the runtime's optional interrupt/RPC/sandbox interfaces.
// Retry stays inside Manager.Start, so the canonical session, binding and
// idempotency result never point at an abandoned first attempt.
type recoveryRuntime struct {
	agentsessions.Runtime
	service *Service
	plan    *launch.Plan
	id      string
	request context.Context
}

type recoveryUnconfirmedError struct {
	err error
	pid int
}

func (e *recoveryUnconfirmedError) Error() string {
	return "native recovery teardown unconfirmed: " + e.err.Error()
}
func (e *recoveryUnconfirmedError) Unwrap() error { return e.err }

func (r *recoveryRuntime) Start(ctx context.Context, opts agentsessions.StartOptions) (agentsessions.Session, error) {
	// Recovery submits one control turn itself. Suppress the adapter's automatic
	// kickoff rather than submit the boot context twice. Planted policy remains.
	opts.AutoFireFirstTurn = false
	opts.FirstTurnPayload = nil
	opts.BootPrompt = ""
	opts.BootMode = "none"
	nativeID := opts.SessionIDPreset
	started := time.Now()
	sess, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		if nativeID == "" || !errors.Is(err, provider.ErrProviderSessionLost) {
			return nil, err
		}
		return r.cold(ctx, opts, nativeID)
	}
	if nativeID == "" {
		r.kickoff(ctx, sess, opts)
		return sess, nil
	}

	done := make(chan error, 1)
	go func() {
		code, waitErr := sess.Wait()
		if waitErr == nil {
			waitErr = fmt.Errorf("native process ended during recovery (exit=%d)", code)
		}
		done <- waitErr
	}()
	first := r.kickoff(ctx, sess, opts)
	grace := r.service.resumeGrace
	if grace <= 0 {
		grace = nativeResumeGrace
	}
	timer := time.NewTimer(grace - time.Since(started))
	defer timer.Stop()
	for {
		select {
		case <-r.request.Done():
			if err := stopRecoveryAttempt(sess, done); err != nil {
				return nil, &recoveryUnconfirmedError{err: err, pid: sess.Health().PID}
			}
			return nil, r.request.Err()
		case <-timer.C:
			return sess, nil
		case err = <-done:
			if errors.Is(err, provider.ErrProviderNotAuthenticated) {
				return nil, err
			}
			return r.cold(ctx, opts, nativeID)
		case turnErr := <-first:
			first = nil
			if turnErr == nil {
				// Per-turn providers have proven this native ID. Long-lived
				// SendInput only acknowledges submission; still watch their child.
				if !r.Caps().StreamingStdio && !r.Caps().PTY && !r.Caps().JsonRpcStdio {
					return sess, nil
				}
				continue
			}
			var exitErr *exec.ExitError
			lost := errors.Is(turnErr, provider.ErrProviderSessionLost)
			fastDeath := errors.As(turnErr, &exitErr) && !errors.Is(turnErr, provider.ErrProviderNotAuthenticated)
			if !lost && !fastDeath {
				if err := stopRecoveryAttempt(sess, done); err != nil {
					return nil, &recoveryUnconfirmedError{err: err, pid: sess.Health().PID}
				}
				return nil, turnErr
			}
			if err := stopRecoveryAttempt(sess, done); err != nil {
				return nil, &recoveryUnconfirmedError{err: err, pid: sess.Health().PID}
			}
			return r.cold(ctx, opts, nativeID)
		}
	}
}

func stopRecoveryAttempt(sess agentsessions.Session, done <-chan error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := sess.Stop(ctx); err != nil {
		return err
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (r *recoveryRuntime) cold(ctx context.Context, opts agentsessions.StartOptions, nativeID string) (agentsessions.Session, error) {
	if err := r.request.Err(); err != nil {
		return nil, err
	}
	if err := r.service.Store.ClearNativeResume(r.request, r.plan.ResumeSourceSessionID, r.id, r.plan.ProviderID, nativeID); err != nil {
		return nil, fmt.Errorf("fence failed native ID: %w", err)
	}
	r.service.codexThreads.Forget(r.id)
	r.plan.ResumeProviderSessionID = ""
	opts.SessionIDPreset = ""
	publishSessionEvent(r.service.Bus, r.id, r.plan.LogicalAgentID, events.KindProviderSessionLost, struct {
		Requested         string `json:"requested"`
		Reason            string `json:"reason"`
		FreshConversation bool   `json:"fresh_conversation"`
	}{nativeID, "initial native recovery failed; cold boot with recovery pack", true})
	sess, err := r.Runtime.Start(ctx, opts)
	if err != nil {
		return nil, err
	}
	r.kickoff(ctx, sess, opts)
	return sess, nil
}

func (r *recoveryRuntime) kickoff(ctx context.Context, sess agentsessions.Session, opts agentsessions.StartOptions) <-chan error {
	result := make(chan error, 1)
	go func() {
		text := r.plan.BootPrompt
		if r.plan.RecoveryPrompt != "" {
			text = r.plan.RecoveryPrompt
		}
		if r.Caps().JsonRpcStdio && r.plan.ProviderBrand == "codex" {
			rpc, ok := sess.(agentsessions.JsonRpcCaller)
			if !ok {
				result <- fmt.Errorf("codex recovery session has no RPC channel")
				return
			}
			err := r.service.codexThreads.SendTurn(ctx, r.id, rpc, text, turn.CodexAppServerOptions{
				ClientName: "tether", ClientVersion: tetherClientVersion, CWD: r.plan.EffectiveWorkRoot(), ResumeThreadID: opts.SessionIDPreset,
			})
			if id, bound := r.service.codexThreads.ThreadID(r.id); bound && opts.OnSessionID != nil {
				opts.OnSessionID(id)
			}
			if err == nil {
				_ = r.service.Store.AdvanceRecoveryCursors(ctx, recoveryCursorKey(r.plan), r.plan.RecoveryCursors)
			}
			result <- err
			return
		}
		frame, err := turn.Frame(text, turn.Options{Provider: r.plan.ProviderBrand, Runtime: recoveryMode(r.Caps())})
		if err == nil {
			err = sess.SendInput(ctx, frame)
		}
		if err == nil {
			_ = r.service.Store.AdvanceRecoveryCursors(ctx, recoveryCursorKey(r.plan), r.plan.RecoveryCursors)
		}
		result <- err
	}()
	return result
}

func recoveryMode(caps agentsessions.Capabilities) runtimes.Mode {
	if caps.StreamingStdio {
		return runtimes.ModeStreamingStdio
	}
	if caps.PTY {
		return runtimes.ModePTY
	}
	return runtimes.ModeSubprocessPerTurn
}
