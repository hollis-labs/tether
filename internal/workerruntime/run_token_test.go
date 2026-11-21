package workerruntime

import (
	"github.com/hollis-labs/tether/internal/workerwire"
	"net/http"
	"strings"
	"sync"
	"testing"
)

type runCaptureTransport struct {
	mu      sync.Mutex
	methods map[string]int
	token   string
	bad     bool
}

func (c *runCaptureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.methods[r.Method]++
	if r.Header.Get("X-Run-Token") != c.token {
		c.bad = true
	}
	c.mu.Unlock()
	return http.DefaultTransport.RoundTrip(r)
}
func TestRunTokenRelayAllMethodsAndNoSecretDiagnostics(t *testing.T) {
	const secret = "synthetic-run-token-never-log"
	capture := &runCaptureTransport{methods: map[string]int{}, token: secret}
	g := newRunRelayRig(t, &fakeHub{}, capture, secret)
	g.handshake()
	_ = g.recv()
	g.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	_ = g.recv()
	g.finish()
	capture.mu.Lock()
	defer capture.mu.Unlock()
	if capture.bad || capture.methods["POST"] == 0 || capture.methods["GET"] == 0 || capture.methods["DELETE"] == 0 {
		t.Fatalf("run header lost on relay: %+v", capture.methods)
	}
	if strings.Contains(g.stderr.String(), secret) {
		t.Fatal("run token in diagnostics")
	}
}

func TestWorkerAdvertisesExecutionHandoffOnlyWhenSupported(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		found := false
		for _, feature := range workerFeatures(enabled) {
			if feature == workerwire.FeatureRunTokenFile {
				found = true
			}
		}
		if found != enabled {
			t.Fatal("incorrect execution handoff feature advertisement")
		}
	}
}
