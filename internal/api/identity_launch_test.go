package api

import (
	"context"
	"github.com/hollis-labs/tether/internal/identity"
	"net/http"
	"net/http/httptest"
	"testing"
)

type contextualLaunchService struct {
	*fakeLaunchService
	principal string
}

func (s *contextualLaunchService) LaunchSessionWithContext(ctx context.Context, id string) (LaunchResult, error) {
	p, _ := identity.FromContext(ctx)
	s.principal = p.ID
	return s.LaunchSession(id)
}
func (s *contextualLaunchService) ResumeLogicalAgentWithContext(ctx context.Context, id string, opts ResumeOptions) (LaunchResult, error) {
	p, _ := identity.FromContext(ctx)
	s.principal = p.ID
	return s.ResumeLogicalAgent(id, opts)
}
func TestLaunchAndResumeCarryVerifiedPrincipal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, path := range []string{"/sessions/s/launch", "/logical-agents/a/resume"} {
		t.Run(path, func(t *testing.T) {
			svc := &contextualLaunchService{fakeLaunchService: &fakeLaunchService{launchRes: LaunchResult{SessionID: "s"}, resumeRes: LaunchResult{SessionID: "s"}}}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			req = req.WithContext(identity.WithPrincipal(req.Context(), identity.Principal{ID: "verified-parent"}))
			rr := httptest.NewRecorder()
			newCheckpointTestHandler(svc, &fakeCheckpoints{}).ServeHTTP(rr, req)
			if rr.Code != http.StatusOK && rr.Code != http.StatusCreated {
				t.Fatalf("status=%d", rr.Code)
			}
			if svc.principal != "verified-parent" {
				t.Fatal("verified principal lost across launch/resume API")
			}
		})
	}
}
