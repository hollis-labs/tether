package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/testutil/migrateddb"
)

type grantHealthService struct {
	fakeLaunchService
	seen string
}

func (s *grantHealthService) MCPGrantHealth(_ context.Context, id string) *config.SessionMCPGrantImpact {
	s.seen = id
	return &config.SessionMCPGrantImpact{SessionID: id, Servers: []config.MCPGrantIssue{{Server: "removed", Reason: "removed"}}, Hint: "resume or relaunch"}
}

func TestWhoamiReportsOnlyAuthenticatedSessionGrantHealth(t *testing.T) {
	reg := registry.NewService(registry.NewStorage(migrateddb.Open(t)))
	svc := &grantHealthService{}
	h := NewHandler(Deps{Registry: reg, Service: svc})
	for _, verified := range []bool{true, false} {
		req := httptest.NewRequest("GET", "/whoami?as=msg://agent/local/someone-else", nil)
		if verified {
			req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{Verified: true, ID: "principal", Kind: "session", SessionID: "actual"}))
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var out whoamiResponse
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		if verified {
			if out.MCPGrant == nil || out.MCPGrant.SessionID != "actual" || svc.seen != "actual" {
				t.Fatalf("health=%+v seen=%s", out.MCPGrant, svc.seen)
			}
		} else if out.MCPGrant != nil {
			t.Fatal("unverified caller received grant health")
		}
	}
}
