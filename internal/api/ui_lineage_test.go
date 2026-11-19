package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/store"
)

type lineageReadService struct {
	*fakeLaunchService
	proof *store.ReplacementLineage
	err   error
}

func (s lineageReadService) GetReplacementLineage(context.Context, string) (*store.ReplacementLineage, error) {
	return s.proof, s.err
}
func (s lineageReadService) GetWorkerLineage(context.Context, string) (*store.WorkerLineage, error) {
	return nil, nil
}

func TestSessionItemReplacementLineageProjectionAndFailure(t *testing.T) {
	for _, bad := range []bool{false, true} {
		svc := lineageReadService{fakeLaunchService: &fakeLaunchService{getRes: map[string]*store.SessionRow{"next": {ID: "next", LogicalAgentID: "seat", State: "running", Intent: "resume"}}}, proof: &store.ReplacementLineage{Kind: "retained_team_v1", SourceSessionID: "old", ReplacementSessionID: "next", LogicalAgentID: "seat", ActorURI: "actor", SourceActorURI: "actor", ReplacementActorURI: "actor"}}
		if bad {
			svc.err = errors.New("private custody payload must not be returned")
		}
		w := httptest.NewRecorder()
		newTestHandler(svc).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions/next", nil))
		if bad {
			if w.Code != http.StatusInternalServerError || strings.Contains(w.Body.String(), "private custody") || strings.Contains(w.Body.String(), "retained_team_v1") {
				t.Fatalf("failed proof read claimed success: %d %s", w.Code, w.Body.String())
			}
		} else {
			var dto SessionDTO
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &dto) != nil || dto.ReplacementLineage == nil || dto.ReplacementLineage.Kind != "retained_team_v1" || dto.NativeAdmission != nil {
				t.Fatalf("replacement projection=%d %s", w.Code, w.Body.String())
			}
		}

	}
}
