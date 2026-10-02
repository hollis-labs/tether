package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProbeMCPAdmissionWithoutInitialization(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		healthy bool
	}{
		{"ready", 400, `{"error":{"code":"mcp_session_required"}}`, true},
		{"disabled", 404, `not found`, false},
		{"identity off", 503, `{"error":{"code":"identity_unavailable"}}`, false},
		{"unverified", 401, `{"error":{"code":"unauthorized"}}`, false},
		{"other bad request", 400, `{"error":{"code":"invalid_mcp_selectors"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.Path != "/mcp" || r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("Authorization") != "Bearer canary" {
					t.Error("unexpected probe request")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer stub.Close()
			err := New("tcp:"+strings.TrimPrefix(stub.URL, "http://"), WithToken("canary")).ProbeMCP(context.Background())
			if (err == nil) != tc.healthy {
				t.Fatalf("probe: %v", err)
			}
			if err != nil && strings.Contains(err.Error(), "canary") {
				t.Fatal("credential in error")
			}
		})
	}
}
