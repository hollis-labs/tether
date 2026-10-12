package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/hollis-labs/tether/internal/environment"
)

// A remote listener runs environment.ProtocolGate with require=true. These tests
// pin that the --target client satisfies it and that local callers are unchanged.

func protocolHeaderServer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var seen []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Header.Get(environment.ProtocolHeader))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"sessions":[]}`))
	})
	// Same gate the remote listener installs: header required.
	return httptest.NewServer(environment.ProtocolGate(true, h)), &seen
}

func TestWithProtocolHeaderSatisfiesRemoteGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	srv, seen := protocolHeaderServer(t)
	defer srv.Close()
	c := New("tcp:"+srv.Listener.Addr().String(), WithToken("tth_test"), WithProtocolHeader())
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.baseURL+"/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d, want 200 (gate must admit the versioned client)", resp.StatusCode)
	}
	if len(*seen) != 1 || (*seen)[0] != strconv.Itoa(environment.Protocol) {
		t.Fatalf("protocol header seen %v, want [%d]", *seen, environment.Protocol)
	}
}

func TestWithoutProtocolHeaderIsRefusedByRemoteGate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TETHER_TOKEN", "")
	srv, seen := protocolHeaderServer(t)
	defer srv.Close()
	c := New("tcp:"+srv.Listener.Addr().String(), WithToken("tth_test"))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.baseURL+"/sessions", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status %d, want 409 protocol_mismatch for a client that omits the header", resp.StatusCode)
	}
	if len(*seen) != 0 {
		t.Fatalf("handler ran without the header: %v", *seen)
	}
}
