package client

import (
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
