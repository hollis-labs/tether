package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestRunTokenTransportClonesAndSeparatesHeaders(t *testing.T) {
	base := &recordingRT{}
	rt := mcpErrorTransport{base: base, clientLabel: "label", runToken: "synthetic-run-secret"}
	for _, method := range []string{http.MethodPost, http.MethodGet, http.MethodDelete} {
		req := httptest.NewRequest(method, "http://unix/mcp", nil)
		req.Header.Set("Authorization", "Bearer synthetic-bearer")
		if _, err := rt.RoundTrip(req); err != nil {
			t.Fatal(err)
		}
		if base.seen.Header.Get("X-Run-Token") != "synthetic-run-secret" || base.seen.Header.Get("Authorization") != "Bearer synthetic-bearer" || base.seen.Header.Get("X-Tether-Client-Label") != "label" {
			t.Fatal("lost distinct headers")
		}
		if req.Header.Get("X-Run-Token") != "" {
			t.Fatal("mutated original request")
		}
	}
}
func TestRunTokenClientRefusesRedirectWithoutBearer(t *testing.T) {
	var leaked atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Add(1); w.WriteHeader(401) }))
	defer sink.Close()
	var presented atomic.Bool
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented.Store(r.Header.Get("X-Run-Token") == "synthetic-run-secret")
		http.Redirect(w, r, sink.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()
	c := &Client{baseURL: origin.URL, http: origin.Client()}
	s, err := c.ConnectMCP(context.Background(), MCPOptions{RunToken: "synthetic-run-secret"})
	if s != nil {
		_ = s.Close()
	}
	if err == nil || !presented.Load() || leaked.Load() != 0 {
		t.Fatal("run token redirect boundary failed")
	}
}
