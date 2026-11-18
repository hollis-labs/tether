package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/identity"
)

// Exercise the production router: an injected backend cannot accidentally
// become a route on either TCP listener, even when a bearer would pass auth.
func TestMCPBackendMountIsLocalUnixOnly(t *testing.T) {
	for _, tc := range []struct {
		name, addr string
		remote     bool
		want       bool
	}{
		{"unix", "unix:/unused.sock", false, true},
		{"local TCP", "tcp:127.0.0.1:9000", false, false},
		{"remote listener", "unix:/unused.sock", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached := false
			s := &Server{Config: Config{ListenAddr: tc.addr, IdentityMode: identity.Off}, MCPBackend: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true; w.WriteHeader(204) })}
			defer s.CloseIdentityAudit()
			h := s.handler(tc.remote)
			for _, path := range []string{"/gateway/mcp/admit", "/gateway/mcp/remote/admit", "/gateway/mcp/native", "/gateway/mcp/remote/native"} {
				reached = false
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
				if reached != tc.want {
					t.Fatalf("%s reached=%v want=%v", path, reached, tc.want)
				}
			}
		})
	}
}
