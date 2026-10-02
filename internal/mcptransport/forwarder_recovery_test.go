package mcptransport

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/mcpforward"
	"github.com/hollis-labs/tether/internal/mcpgateway"
	"github.com/hollis-labs/tether/internal/store"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func thinSession(ctx context.Context, t *testing.T, f *transportFixture, id string) *mcp.ClientSession {
	t.Helper()
	if err := f.db.CreateSession(store.SessionRow{ID: id, LogicalAgentID: "agent-" + id, State: "running"}, &launch.Plan{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SaveSessionMCPPolicy(ctx, mcpgateway.SessionPolicy{SessionID: id, AgentID: "agent-" + id, Servers: []string{"app"}}.Seal()); err != nil {
		t.Fatal(err)
	}
	token, err := f.ids.Mint(ctx, identity.Principal{ID: "session:" + id, Kind: "session", SessionID: id})
	if err != nil {
		t.Fatal(err)
	}
	left, right := mcp.NewInMemoryTransports()
	done := make(chan error, 1)
	go func() {
		done <- mcpforward.Run(ctx, client.New(f.addr, client.WithToken(token)), client.MCPOptions{}, right)
	}()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "recovery", Version: "1"}, nil).Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close(); <-done })
	return session
}

func waitFixtureFile(ctx context.Context, t *testing.T, path string) {
	t.Helper()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("fixture never reached its execution barrier", ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestThinForwarderIdleRecoveryPreservesOtherCalls(t *testing.T) {
	f := newTransportFixtureWithTimeout(t, true, true, 2*time.Minute)
	bound := 45 * time.Second
	longIdle := os.Getenv("TETHER_FORWARDER_IDLE_PROBE") == "1"
	if longIdle {
		bound = 210 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), bound)
	defer cancel()
	alpha := thinSession(ctx, t, f, "recovery-alpha")
	beta := thinSession(ctx, t, f, "recovery-beta")
	if longIdle {
		// Opt-in real elapsed-time probe; ordinary gate uses the same expiry path
		// with a deterministic timestamp, without adding two minutes to the suite.
		select {
		case <-time.After(131 * time.Second):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	} else {
		f.handler.mu.Lock()
		for v := range f.handler.views {
			v.lastUsed = time.Now().Add(-131 * time.Second)
		}
		f.handler.mu.Unlock()
		for {
			f.handler.mu.Lock()
			expired := len(f.handler.sessions) == 0
			f.handler.mu.Unlock()
			if expired {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("idle views did not expire", ctx.Err())
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	slow := make(chan error, 1)
	go func() {
		result, err := alpha.CallTool(ctx, &mcp.CallToolParams{Name: "app_slow"})
		if err == nil && result.IsError {
			err = fmt.Errorf("slow call failed: %+v", result)
		}
		slow <- err
	}()
	waitFixtureFile(ctx, t, f.childStarts+".call")
	result, err := beta.CallTool(ctx, &mcp.CallToolParams{Name: "app_echo", Arguments: map[string]any{"message": "beta recovered"}})
	if err != nil || result.IsError {
		t.Fatalf("idle forwarder failed recovery: %+v %v", result, err)
	}
	if err := <-slow; err != nil {
		t.Fatal("another view's recovery retired an in-flight call", err)
	}
	var group sync.WaitGroup
	for i := range 8 {
		for _, session := range []*mcp.ClientSession{alpha, beta} {
			group.Go(func() {
				result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "app_echo", Arguments: map[string]any{"message": fmt.Sprint(i)}})
				if err != nil || result.IsError {
					t.Errorf("concurrent call: %+v %v", result, err)
				}
			})
		}
	}
	group.Wait()
}

func TestThinForwarderEndpointLossNeverReplaysSideEffect(t *testing.T) {
	f := newTransportFixture(t, true, true)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	session := thinSession(ctx, t, f, "effect-session")
	result := make(chan error, 1)
	go func() { _, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "app_count"}); result <- err }()
	waitFixtureFile(ctx, t, f.childStarts+".effects")
	if err := f.closeHTTP(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.childStarts+".release", []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		var protocol *jsonrpc.Error
		if !errors.As(err, &protocol) || protocol.Code != -32001 || !strings.Contains(protocol.Message, "daemon_unreachable") {
			t.Fatalf("lost endpoint must return typed error: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("lost endpoint left tool call hanging")
	}
	raw, err := os.ReadFile(f.childStarts + ".effects")
	if err != nil || string(raw) != "effect\n" {
		t.Fatalf("side effect replayed: %q %v", raw, err)
	}
}
