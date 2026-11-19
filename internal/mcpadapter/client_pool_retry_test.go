package mcpadapter

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/supervise"

	"github.com/hollis-labs/tether/internal/config"
)

// flakyUpstream is an http MCP upstream that answers 503 until up is set:
// a service restarting when the hub first reaches it.
func flakyUpstream(t *testing.T) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	upstream := gomcp.NewServer("upstream", "test")
	registerTestTool(upstream, "probe")
	handler := mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server { return upstream.SDKServer() }, nil)
	up := &atomic.Bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !up.Load() {
			http.Error(w, "restarting", http.StatusServiceUnavailable)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, up
}

func remoteRetryTiming(t *testing.T, base, maxDelay time.Duration) {
	t.Helper()
	oldBase, oldMax, oldJitter := remoteRetryBase, remoteRetryMax, remoteRetryJitter
	remoteRetryBase, remoteRetryMax, remoteRetryJitter = base, maxDelay, 0
	t.Cleanup(func() { remoteRetryBase, remoteRetryMax, remoteRetryJitter = oldBase, oldMax, oldJitter })
}

func startFlakyPool(t *testing.T, url string) (*ClientPool, func() []string) {
	t.Helper()
	pool := NewClientPool([]config.MCPServerEntry{{ID: "tangent", Transport: "http", URL: url}}, NewToolRegistry())
	pool.policy.handshakeTimeout = 2 * time.Second
	var mu sync.Mutex
	var seen []string
	pool.SetStateHandler(func(c UpstreamStateChange) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, c.To)
		if c.To == "recovering" && c.NextRetryAt == nil {
			t.Errorf("reconnecting with no next attempt: %+v", c)
		}
	})
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Shutdown)
	return pool, func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(seen) }
}

func statusOf(pool *ClientPool, id string) ServerStatus {
	for _, s := range pool.StatusSummary() {
		if s.ID == id {
			return s
		}
	}
	return ServerStatus{}
}

func waitConnected(t *testing.T, pool *ClientPool, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if s := statusOf(pool, "tangent"); s.Status == "connected" && s.ToolCount == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("not connected after %s: %+v", within, statusOf(pool, "tangent"))
}

// CW-20261114-0182: an http upstream down or restarting at the hub's first
// handshake was marked failed and never retried; only a hub reload brought it
// back. It now retries with backoff and comes up on its own.
func TestRemoteUpstreamDownAtStartRecoversOnItsOwn(t *testing.T) {
	remoteRetryTiming(t, 50*time.Millisecond, 200*time.Millisecond)
	srv, up := flakyUpstream(t)
	pool, seen := startFlakyPool(t, srv.URL)
	s := statusOf(pool, "tangent")
	if s.Status == "connected" {
		t.Fatalf("connected to a down upstream: %+v", s)
	}
	time.Sleep(300 * time.Millisecond) // several failed attempts
	if s = statusOf(pool, "tangent"); s.Status != "recovering" && s.Status != "starting" && s.Status != "failed" {
		t.Fatalf("down upstream state %+v", s)
	}
	up.Store(true)
	waitConnected(t, pool, 2*time.Second)
	got := seen()
	for _, want := range []string{"failed", "recovering", "starting", "connected"} {
		if !slices.Contains(got, want) {
			t.Fatalf("transitions %v lack %s", got, want)
		}
	}
	if s = statusOf(pool, "tangent"); s.NextRetryAt != nil || s.RestartAttempts != 0 {
		t.Fatalf("connected status still carries a retry: %+v", s)
	}
}

// While it waits out its backoff, a down upstream shows the retry and its
// next attempt. A refresh (explicit) re-probes it at once instead of reporting
// the stale failure; a tool list or call (implicit Retry) does not jump a long
// backoff early in its step.
func TestDownRemoteUpstreamIsReprobedOnDemand(t *testing.T) {
	for _, how := range []string{"refresh", "retry"} {
		t.Run(how, func(t *testing.T) {
			remoteRetryTiming(t, time.Hour, time.Hour)
			srv, up := flakyUpstream(t)
			pool, _ := startFlakyPool(t, srv.URL)
			deadline := time.Now().Add(2 * time.Second)
			for statusOf(pool, "tangent").Status != "recovering" && time.Now().Before(deadline) {
				time.Sleep(10 * time.Millisecond)
			}
			s := statusOf(pool, "tangent")
			if s.Status != "recovering" || s.NextRetryAt == nil || time.Until(*s.NextRetryAt) < 30*time.Minute || s.Error == "" {
				t.Fatalf("waiting status: %+v", s)
			}
			up.Store(true)
			switch how {
			case "refresh":
				res, err := pool.RefreshServer(context.Background(), "tangent")
				if err != nil || res.ToolCount != 1 {
					t.Fatalf("refresh: %+v %v", res, err)
				}
			case "retry":
				if pool.Retry("tangent") {
					t.Fatal("an implicit retry jumped an hour-long backoff at its start")
				}
				if pool.RetryNow("tangent") != true {
					t.Fatal("explicit retry refused")
				}
			}
			waitConnected(t, pool, 2*time.Second)
		})
	}
}

func TestRemoteRetryDelayBacksOffWithJitterToACap(t *testing.T) {
	for n := range 12 {
		d := remoteRetryDelay(n)
		lo := time.Duration(float64(min(remoteRetryBase<<n, remoteRetryMax)) * (1 - remoteRetryJitter))
		hi := time.Duration(float64(min(remoteRetryBase<<n, remoteRetryMax)) * (1 + remoteRetryJitter))
		if n > 10 {
			lo, hi = time.Duration(float64(remoteRetryMax)*(1-remoteRetryJitter)), time.Duration(float64(remoteRetryMax)*(1+remoteRetryJitter))
		}
		if d < lo || d > hi {
			t.Fatalf("attempt %d: %s outside [%s, %s]", n, d, lo, hi)
		}
	}
}

// transitions records the To of every reported change for id.
type transitions struct {
	mu   sync.Mutex
	list []UpstreamStateChange
}

func (r *transitions) handler(c UpstreamStateChange) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.list = append(r.list, c)
}

func (r *transitions) count(id, to string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, c := range r.list {
		if c.ServerID == id && c.To == to {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, within time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// #581 review (1): implicit retries (tool list, search, call) respect the
// backoff: none before half of a step's wait, at most one per step, however
// often they are asked for, so a dead upstream is never redialed back to back.
func TestImplicitRetryRespectsTheBackoff(t *testing.T) {
	remoteRetryTiming(t, 2*time.Second, time.Minute)
	srv, _ := flakyUpstream(t)
	pool := NewClientPool([]config.MCPServerEntry{{ID: "tangent", Transport: "http", URL: srv.URL}}, NewToolRegistry())
	rec := &transitions{}
	pool.SetStateHandler(rec.handler)
	if err := pool.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Shutdown)
	waitFor(t, "recovering", 2*time.Second, func() bool { return statusOf(pool, "tangent").Status == "recovering" })
	for range 50 {
		if pool.Retry("tangent") {
			t.Fatal("implicit retry before half the step's wait")
		}
	}
	next := *statusOf(pool, "tangent").NextRetryAt
	time.Sleep(time.Until(next.Add(-900 * time.Millisecond)))
	if !pool.Retry("tangent") {
		t.Fatalf("implicit retry refused past half the step: %+v", statusOf(pool, "tangent"))
	}
	for range 50 {
		if pool.Retry("tangent") {
			t.Fatal("a second implicit retry in the same step")
		}
	}
	waitFor(t, "the early attempt", 2*time.Second, func() bool { return attemptsOf(pool, "tangent") == 2 })
	waitFor(t, "the next step", 2*time.Second, func() bool {
		s := statusOf(pool, "tangent")
		return s.Status == "recovering" && s.RestartAttempts == 1
	})
	for range 50 {
		pool.Retry("tangent")
	}
	time.Sleep(200 * time.Millisecond)
	if n := attemptsOf(pool, "tangent"); n != 2 {
		t.Fatalf("%d attempts; implicit retries redialed a dead upstream", n)
	}
	if s := statusOf(pool, "tangent"); s.NextRetryAt == nil || time.Until(*s.NextRetryAt) < 3*time.Second {
		t.Fatalf("the next step did not back off further: %+v", s)
	}
}

// #581 review (2): a remote upstream that fails after connecting gets the
// timed retry too, and comes back on its own.
func TestMidLifeRemoteFailureRetriesOnATimer(t *testing.T) {
	remoteRetryTiming(t, 50*time.Millisecond, 200*time.Millisecond)
	srv, up := flakyUpstream(t)
	up.Store(true)
	pool, _ := startFlakyPool(t, srv.URL)
	waitConnected(t, pool, 2*time.Second)
	up.Store(false)
	if _, err := pool.RefreshServer(context.Background(), "tangent"); err == nil {
		t.Fatal("refresh of a down upstream succeeded")
	}
	waitFor(t, "recovering after a mid-life failure", 2*time.Second, func() bool {
		s := statusOf(pool, "tangent")
		return s.Status == "recovering" && s.NextRetryAt != nil
	})
	up.Store(true)
	waitConnected(t, pool, 2*time.Second) // by the timer: nothing else asks
}

// #581 review (3): the stdio restart (reconnecting), the stdio restart limit
// (failed) and a reconnect accepted with a name collision (connected) are
// reported like every other transition.
func TestEveryTransitionIsReported(t *testing.T) {
	t.Run("stdio", func(t *testing.T) {
		pool := NewClientPool([]config.MCPServerEntry{{ID: "leaf", Transport: "stdio", Command: "unused"}}, NewToolRegistry())
		pool.policy.Policy = supervise.Policy{Delays: []time.Duration{10 * time.Millisecond}}
		pool.SetConnectFunc(func(context.Context, config.MCPServerEntry) (upstreamClient, error) {
			return nil, errors.New("spawn failed")
		})
		rec := &transitions{}
		pool.SetStateHandler(rec.handler)
		if err := pool.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer pool.Shutdown()
		waitFor(t, "restart limit", 2*time.Second, func() bool { return statusOf(pool, "leaf").RecoveryExhausted })
		if rec.count("leaf", "reconnecting") != 1 || rec.count("leaf", "failed") != 3 {
			t.Fatalf("transitions %+v", rec.list)
		}
		rec.mu.Lock()
		last := rec.list[len(rec.list)-1]
		rec.mu.Unlock()
		if last.To != "failed" || !strings.Contains(last.Error, "restart limit") {
			t.Fatalf("restart-limit failure not reported: %+v", last)
		}
	})
	t.Run("collision rebind", func(t *testing.T) {
		remoteRetryTiming(t, 10*time.Millisecond, 50*time.Millisecond)
		var betaTool atomic.Value
		betaTool.Store("b_read")
		pool := NewClientPool([]config.MCPServerEntry{{ID: "alpha", Transport: "http", URL: "http://alpha"}, {ID: "beta", Transport: "http", URL: "http://beta"}}, NewToolRegistry())
		pool.SetConnectFunc(func(_ context.Context, e config.MCPServerEntry) (upstreamClient, error) {
			return &mockClient{listToolsFunc: func(context.Context, *mcpsdk.ListToolsParams) (*mcpsdk.ListToolsResult, error) {
				name := "a_read"
				if e.ID == "beta" {
					name = betaTool.Load().(string)
				}
				return &mcpsdk.ListToolsResult{Tools: []*mcpsdk.Tool{makeTool(name)}}, nil
			}}, nil
		})
		rec := &transitions{}
		pool.SetStateHandler(rec.handler)
		if err := pool.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer pool.Shutdown()
		betaTool.Store("a_read") // beta's next inventory collides with alpha's
		pool.mu.Lock()
		client := pool.statuses["beta"].client
		pool.mu.Unlock()
		pool.fail("beta", client, errors.New("connection lost"))
		waitFor(t, "beta rebound", 2*time.Second, func() bool {
			s := statusOf(pool, "beta")
			return s.Status == "connected" && s.Degraded && rec.count("beta", "connected") == 2
		})
	})
}

func attemptsOf(pool *ClientPool, id string) int {
	pool.mu.Lock()
	defer pool.mu.Unlock()
	return pool.statuses[id].attempts
}
