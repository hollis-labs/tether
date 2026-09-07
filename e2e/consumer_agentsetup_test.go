package e2e

// consumer_agentsetup_test.go — T11's "agent-setup shell" fixture
// consumer. agent-setup itself is a separate repository this task must
// not edit; this simulates exactly the sequence its own launch/host
// boundary hook would perform against Tether's real public surface
// (internal/api/session_bootstrap.go's own doc comment names agent-setup
// explicitly as this endpoint's intended caller): a shell-style launcher
// bootstraps a preassigned SESSION id with a provider-native mapping at
// process-launch time, and a SECOND hook invocation for the SAME session
// (e.g. a retry, or the same launcher script re-run) must not invent a
// competing identity.

import (
	"testing"

	"github.com/hollis-labs/tether/internal/client"
)

func TestConsumer_AgentSetup_BootstrapAtLaunchBoundaryIsIdempotent(t *testing.T) {
	d := StartFixtureDaemon(t)
	c := d.Client()

	const preassignedSessionID = "sess-agent-setup-e2e-0001"
	req := client.SessionBootstrapRequest{
		SessionID: preassignedSessionID,
		Intent:    "preassigned",
		ProviderMappings: []client.SessionBootstrapProviderMapping{
			{Owner: "agent-setup", Provider: "claude-code", NativeSessionID: "native-abc123"},
		},
	}

	first, err := c.BootstrapSession(ctx(), req)
	if err != nil {
		t.Fatalf("bootstrap (first hook invocation): %v", err)
	}
	if !first.Created || first.SessionID != preassignedSessionID {
		t.Fatalf("first bootstrap = %+v, want Created=true SessionID=%q", first, preassignedSessionID)
	}

	// A second hook invocation for the SAME session -- e.g. agent-setup's
	// own script retried after a transient network blip -- must be a
	// no-op on session identity, never a competing/duplicate session.
	second, err := c.BootstrapSession(ctx(), req)
	if err != nil {
		t.Fatalf("bootstrap (second/retry hook invocation): %v", err)
	}
	if second.Created {
		t.Fatal("second bootstrap reported Created=true -- must be idempotent, not invent a competing identity")
	}
	if second.SessionID != preassignedSessionID {
		t.Fatalf("second bootstrap session id = %q, want %q", second.SessionID, preassignedSessionID)
	}

	// The bootstrapped identity is immediately usable for real messaging
	// -- agent-setup's whole point is making the session addressable
	// without a separate registration step.
	whoURN := "msg://session/agent-mux/" + preassignedSessionID
	other := registerAgent(t, c, "e2e-agent-setup-peer")
	sent, err := c.MessageSend(ctx(), sendNotice(t, other, whoURN, "welcome, freshly bootstrapped session"))
	if err != nil {
		t.Fatalf("send to the bootstrapped session's address: %v", err)
	}
	if err := c.MessageConsume(ctx(), sent.ID, whoURN); err != nil {
		t.Fatalf("consume as the bootstrapped session: %v", err)
	}
}
