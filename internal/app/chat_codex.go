package app

import (
	"context"
	"errors"
	"io"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/chatbackend/codexappserver"
	"github.com/hollis-labs/tether/internal/chatwire"
)

// installCodexTap uses the existing stdout fanout, next to the existing log and
// approval handler. Both direct and shim-hosted local runtimes honor Fanout.
func (s *Service) installCodexTap(id, brand string, rt agentsessions.Runtime, opts *agentsessions.StartOptions) *codexappserver.Tap {
	if brand != "codex" || rt == nil || !rt.Caps().JsonRpcStdio {
		return nil
	}
	tap := codexappserver.NewTap()
	if opts.Fanout == nil {
		opts.Fanout = tap
	} else {
		opts.Fanout = io.MultiWriter(opts.Fanout, tap)
	}
	if old, ok := s.codexTaps.Swap(id, tap); ok {
		old.(*codexappserver.Tap).Close()
	}
	return tap
}
func (s *Service) codexTap(id string) *codexappserver.Tap {
	if v, ok := s.codexTaps.Load(id); ok {
		return v.(*codexappserver.Tap)
	}
	return nil
}
func (s *Service) watchCodexTap(id string, tap *codexappserver.Tap) {
	if tap == nil {
		return
	}
	go func() {
		_, _ = s.Manager.WaitSession(context.Background(), id)
		if s.codexTaps.CompareAndDelete(id, tap) {
			tap.Close()
		}
	}()
}
func (s *Service) codexChatBackend(id string) (chatwire.Backend, error) {
	if _, worker := s.workerSession(id); worker {
		return nil, &chatwire.UnsupportedRuntimeError{SessionID: id, Runtime: chatwire.RuntimeCodexAppServer, Reason: "remote worker provider protocol has no turn/interrupt route"}
	}
	if s.Manager == nil {
		return nil, agentsessions.ErrSessionNotRunning
	}
	info, ok := s.Manager.Get(id)
	if !ok || !info.Caps.JsonRpcStdio {
		return nil, agentsessions.ErrSessionNotRunning
	}
	tap := s.codexTap(id)
	if tap == nil {
		return nil, errors.New("codex session has no chat tap; relaunch with this daemon")
	}
	return codexappserver.New(codexappserver.Config{
		Tap: tap,
		Send: func(ctx context.Context, text string) error {
			settle := s.beginTurnInput(ctx, id)
			err := s.trackTurnSubmissionContext(ctx, id, func() error { return s.sendTurnJSONRPCRaw(ctx, id, text) })
			settle(err)
			return err
		},
		Interrupt: func(ctx context.Context, threadID, turnID string) error {
			_, err := s.Manager.JsonRpcCall(ctx, id, "turn/interrupt", map[string]string{"threadId": threadID, "turnId": turnID})
			return err
		},
	}), nil
}
