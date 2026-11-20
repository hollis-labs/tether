package workerruntime

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/hollis-labs/libs/security/secrets/tokenfile"
)

type stoppingRoundTrip func(*http.Request) (*http.Response, error)

func (f stoppingRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func newStoppingRelay(t *testing.T, transport http.RoundTripper) *HTTPRelay {
	t.Helper()
	file := filepath.Join(t.TempDir(), "token")
	if err := tokenfile.Write(file, tokenOne); err != nil {
		t.Fatal(err)
	}
	return &HTTPRelay{URL: "http://localhost/mcp", TokenFile: file, Client: &http.Client{Transport: transport}, session: "old", initMsg: []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`)}
}

func TestHTTPRelaySharedRecoveryBudgetWithSlowHandshakes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		started := make(chan struct{})
		var once sync.Once
		var attempts []time.Duration
		relay := newStoppingRelay(t, stoppingRoundTrip(func(r *http.Request) (*http.Response, error) {
			attempts = append(attempts, time.Since(start))
			once.Do(func() { close(started) })
			<-r.Context().Done()
			return nil, r.Context().Err()
		}))
		results := make(chan bool, 11)
		go func() { results <- relay.reinitialize(context.Background(), "old") }()
		<-started
		for range 10 {
			go func() { results <- relay.reinitialize(context.Background(), "old") }()
		}
		synctest.Wait()
		for range 11 {
			if <-results {
				t.Fatal("offline recovery succeeded")
			}
		}
		want := []time.Duration{0, 16 * time.Second, 33 * time.Second, 52 * time.Second}
		if !reflect.DeepEqual(attempts, want) || time.Since(start) != 60*time.Second {
			t.Fatalf("attempts=%v elapsed=%v", attempts, time.Since(start))
		}
	})
}

func TestHTTPRelayGenericReplacementFailuresDoNotBackoff(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			calls := 0
			relay := newStoppingRelay(t, stoppingRoundTrip(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"generic_failure"}}`)), Header: http.Header{}}, nil
			}))
			if relay.reinitialize(context.Background(), "old") || calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}

func TestHTTPRelayRecoversGracefulStopBeforeToolDispatch(t *testing.T) {
	var toolRejected atomic.Bool
	var initAttempts atomic.Int32
	transport := stoppingRoundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			raw, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(raw))
			stopping := false
			if methodName(raw) == "tools/call" {
				stopping = toolRejected.CompareAndSwap(false, true)
			}
			if methodName(raw) == "initialize" && toolRejected.Load() {
				stopping = initAttempts.Add(1) == 1
			}
			if stopping {
				return &http.Response{StatusCode: 503, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"mcp_stopping"}}`))}, nil
			}
		}
		return http.DefaultTransport.RoundTrip(r)
	})
	hub := &fakeHub{}
	rig := newRelayRig(t, hub, transport)
	rig.handshake()
	rig.send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"write"}}`)
	reply := rig.recvReply()
	if reply["error"] != nil || reply["result"] == nil || hub.count("tools/call") != 1 || initAttempts.Load() != 2 {
		t.Fatalf("reply=%v dispatch=%d init=%d", reply, hub.count("tools/call"), initAttempts.Load())
	}
	rig.finish()
}

func TestHTTPRelayInitializedReadiness(t *testing.T) {
	for _, mode := range []string{"transient", "exhausted", "unauthorized"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				attempts := 0
				var relay *HTTPRelay
				relay = newStoppingRelay(t, stoppingRoundTrip(func(req *http.Request) (*http.Response, error) {
					response := &http.Response{StatusCode: 202, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(""))}
					if req.Method == http.MethodGet {
						response.StatusCode = 405
						return response, nil
					}
					raw, _ := io.ReadAll(req.Body)
					if methodName(raw) == "initialize" {
						attempts++
						response.StatusCode = 200
						response.Header.Set("Mcp-Session-Id", "replacement")
						response.Body = io.NopCloser(strings.NewReader(`{"result":{"protocolVersion":"2025-03-26"}}`))
						return response, nil
					}
					relay.mu.Lock()
					session := relay.session
					relay.mu.Unlock()
					if session != "old" || req.Header.Get("Mcp-Session-Id") != "replacement" || req.Header.Get("MCP-Protocol-Version") != "2025-03-26" {
						t.Errorf("candidate published before readiness: session=%s headers=%v", session, req.Header)
					}
					if mode != "transient" || attempts == 1 {
						response.StatusCode = 503
						response.Body = io.NopCloser(strings.NewReader(`{"error":{"code":"mcp_stopping"}}`))
						if mode == "unauthorized" {
							response.StatusCode = 401
						}
					}
					return response, nil
				}))
				relay.out = io.Discard
				ok := relay.reinitialize(context.Background(), "old")
				relay.mu.Lock()
				session := relay.session
				relay.mu.Unlock()
				switch mode {
				case "transient":
					if !ok || session != "replacement" || attempts != 2 || time.Since(start) != time.Second {
						t.Fatalf("ok=%v session=%s attempts=%d elapsed=%v", ok, session, attempts, time.Since(start))
					}
				case "exhausted":
					if ok || session != "old" || attempts != 6 || time.Since(start) != 60*time.Second {
						t.Fatalf("ok=%v session=%s attempts=%d elapsed=%v", ok, session, attempts, time.Since(start))
					}
				case "unauthorized":
					if ok || session != "old" || attempts != 1 {
						t.Fatalf("ok=%v session=%s attempts=%d", ok, session, attempts)
					}
				}
			})
		})
	}
}
