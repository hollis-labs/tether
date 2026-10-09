package acp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters"
	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/substrate/harness/adapters/providertest"
	"github.com/hollis-labs/substrate/llm-core/contracts/runtimes"
)

func TestACPInterruptTurnPreservesSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fake := providertest.New(t, runtimes.Copilot, providertest.Script(
		providertest.Recv(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":1,"agentCapabilities":{},"authMethods":[]}}`),
		providertest.Recv(`{"jsonrpc":"2.0","id":2,"method":"session/new"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":2,"result":{"sessionId":"interrupt-session"}}`),
		providertest.Recv(`{"jsonrpc":"2.0","id":3,"method":"session/prompt"}`),
		providertest.Send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"interrupt-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"running"}}}}`),
		providertest.Recv(`{"jsonrpc":"2.0","method":"session/cancel"}`),
		providertest.Send(`{"jsonrpc":"2.0","id":3,"result":{"stopReason":"cancelled"}}`), //nolint:misspell // ACP wire stop reason
		providertest.Recv(`{"jsonrpc":"2.0","id":4,"method":"session/prompt"}`),
		providertest.Send(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"interrupt-session","update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"next reply"}}}}`),
		providertest.Send(`{"jsonrpc":"2.0","id":4,"result":{"stopReason":"end_turn"}}`),
		providertest.AwaitEOF(), providertest.Exit(0),
	))
	rt, err := New("copilot", "copilot", runtimes.ModeACPStdio, fake.Path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	out := &lockedBuffer{}
	sess, err := rt.Start(ctx, agentsessions.StartOptions{Workdir: t.TempDir(), WorkspaceDir: t.TempDir(), Fanout: out})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sess.Stop(context.Background()) }()
	if !sess.(*session).DeliveryCapabilities().Supports(adapters.DeliveryCapabilityCancelTurn) {
		t.Fatal("ACP cancel_turn not advertised")
	}
	if err := sess.SendInput(ctx, []byte("first")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "running")
	if err := sess.(agentsessions.TurnInterrupter).InterruptTurn(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "[turn_done]")
	for sess.Health().State != agentsessions.LiveStateIdle {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := sess.SendInput(ctx, []byte("replacement")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, out, "next reply")
	if !sess.Health().Alive {
		t.Fatal("interrupt stopped the session")
	}
}

func TestACPInterruptTurnRequiresAdvertisement(t *testing.T) {
	// A wrapper without an adapter has no delivery advertisement.
	s := &session{}
	if err := s.InterruptTurn(context.Background()); !errors.Is(err, agentsessions.ErrInterruptUnsupported) {
		t.Fatalf("interrupt = %v, want ErrInterruptUnsupported", err)
	}
}
