package mcpadapter

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gomcp "github.com/hollis-labs/libs/mcp/plugin-mcp/go-mcp/server"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"

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
// next attempt; a refresh re-probes it at once instead of reporting the stale
// failure, and so does Retry (a tool list or call that needs it).
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
				if !pool.Retry("tangent") {
					t.Fatal("retry not requested")
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
