package e2e

// helpers_test.go — small utilities shared across this package's
// scenario tests: registering an agent through the real registry API,
// and polling for an eventually-true condition (real daemon operations
// like wake-sweep retries or async relay are not synchronous).

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/registry"
)

// registerAgent registers a minimal agent Profile via the real HTTP API
// and returns its minted URN.
func registerAgent(t *testing.T, c *client.Client, displayName string) string {
	t.Helper()
	p, err := c.Registry().Register(context.Background(), registry.KindAgent, registry.Profile{
		DisplayName:   displayName,
		LastUpdatedBy: "e2e-test",
	})
	if err != nil {
		t.Fatalf("register agent %q: %v", displayName, err)
	}
	return p.URN
}

// sendNotice builds a minimal MessageSendRequest for a "notice" kind
// message with a plain text payload -- the shape almost every scenario
// test in this package needs, since the exact payload content is rarely
// what's under test.
func sendNotice(t *testing.T, from, to, text string) client.MessageSendRequest {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return client.MessageSendRequest{Kind: "notice", From: from, To: to, Payload: payload}
}

// messageSendRequestOfKind builds a MessageSendRequest with an explicit
// kind and raw JSON payload, for scenarios (e.g. request/status_update
// task-tracking flows) that need a specific envelope kind rather than a
// plain notice.
func messageSendRequestOfKind(kind, from, to string, payload json.RawMessage) client.MessageSendRequest {
	return client.MessageSendRequest{Kind: kind, From: from, To: to, Payload: payload}
}

// pollUntil retries fn every interval until it returns true, or fails the
// test once timeout elapses. Used for real-daemon async effects (a wake
// sweep tick, a relayed message landing) that have no synchronous signal.
func pollUntil(t *testing.T, timeout, interval time.Duration, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func ctx() context.Context { return context.Background() }
