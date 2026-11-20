package client

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPExpiryResponsePreserved(t *testing.T) {
	for _, fixture := range []struct {
		body    string
		status  int
		missing bool
	}{
		{`{"error":{"code":"mcp_initialize_required"}}`, 400, true},
		{`{"error":{"code":"mcp_stopping"}}`, 503, true},
		{`{"error":{"code":"generic_failure"}}`, 503, false},
		{`{"error":{"code":"session_expired"}}`, 404, true},
		{`{"error":{"code":"mcp_session_not_found"}}`, 404, true},
		{"session not found\n", 404, true},
		{"404 page not found\n", 404, false},
		{`{"error":{"code":"forbidden"}}`, 403, false},
	} {
		t.Run(fixture.body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(fixture.status)
				io.WriteString(w, fixture.body)
			}))
			defer server.Close()
			observed := false
			transport := mcpErrorTransport{base: http.DefaultTransport, onSessionMissing: func() { observed = true }}
			request, _ := http.NewRequest(http.MethodPost, server.URL, nil)
			request.Header.Set("Mcp-Session-Id", "old-session")
			response, err := transport.RoundTrip(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			raw, err := io.ReadAll(response.Body)
			if err != nil || string(raw) != fixture.body || observed != fixture.missing {
				t.Fatalf("body=%q missing=%v err=%v", raw, observed, err)
			}
		})
	}
}

func TestMCPStoppingGETDoesNotProveToolWasRejected(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(503)
		io.WriteString(w, `{"error":{"code":"mcp_stopping"}}`)
	}))
	defer server.Close()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		missing, rejected := false, false
		transport := mcpErrorTransport{base: http.DefaultTransport, onSessionMissing: func() { missing = true }, onStopping: func(context.Context) { rejected = true }}
		request, _ := http.NewRequest(method, server.URL, nil)
		request.Header.Set("Mcp-Session-Id", "old-session")
		response, err := transport.RoundTrip(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if !missing || rejected != (method == http.MethodPost) {
			t.Fatalf("%s missing=%v rejected=%v", method, missing, rejected)
		}
	}
}
