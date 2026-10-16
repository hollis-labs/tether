package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/app/sessionrefs"
	"github.com/hollis-labs/tether/internal/client"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/store"
)

type plantedRefSessions struct{ api.LaunchService }

// This is the separate proxy process's HTTP storage boundary. Record chooses
// source=proxy internally; HTTP transport leaves provenance to the daemon.
type plantedRefAttacher struct{ client *client.Client }

func (a plantedRefAttacher) AttachSessionRef(ctx context.Context, id, kind, refID, uri, relation, _ string, parent string) error {
	_, err := a.client.AttachSessionRef(ctx, id, api.SessionRefAttachRequest{
		Kind: kind, RefID: refID, URI: uri, Relation: relation, ParentItemID: parent,
	})
	return err
}

// Complete the launch test across the capture/persistence boundary. The inputs
// come from the actual planted config, not a second copy of the launch policy.
// SDK dispatch is covered by mcpadapter.TestProxiedCall_ExtractsRefWithProxySource.
func assertPlantedProxyCapture(t *testing.T, db *store.Store, args []string, sessionID string, enabled bool) {
	t.Helper()
	plantedSession := ""
	for i, arg := range args {
		if arg == "--session" && i+1 < len(args) {
			plantedSession = args[i+1]
		}
	}
	handler := api.NewHandler(api.Deps{Service: &plantedRefSessions{}, SessionRefs: db})
	// Supply the authenticated boundary fixture here; the real bearer path is
	// covered by cmd/tether.TestRefAttacherDerivesSourceFromSessionBearer.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := identity.WithPrincipal(r.Context(), identity.Principal{Verified: true, Kind: "session", SessionID: sessionID})
		handler.ServeHTTP(w, r.WithContext(ctx))
	}))
	t.Cleanup(server.Close)
	dc := client.New("tcp:"+strings.TrimPrefix(server.URL, "http://"), client.WithToken(""))
	capture := sessionrefs.New(slices.Contains(args, "--extract-refs"), plantedSession, nil, plantedRefAttacher{client: dc}, nil)
	ctx := context.Background()
	capture.Record(ctx, sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261012-0070"}, Failed: true})
	success := sessionrefs.Observation{ToolName: "torque_task_get", Arguments: map[string]any{"id": "CW-20261012-0069"}}
	capture.Record(ctx, success)
	capture.Record(ctx, success) // retries must not duplicate a recorded reference
	refs, err := dc.ListSessionRefs(ctx, sessionID, "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !enabled {
		if len(refs) != 0 {
			t.Fatalf("disabled launch captured refs: %+v", refs)
		}
		return
	}
	if len(refs) != 1 {
		t.Fatalf("successful capture/retry/failed call: %+v", refs)
	}
	ref := refs[0]
	if ref.SessionID != sessionID || ref.Kind != "torque_task" || ref.RefID != "CW-20261012-0069" || ref.Relation != "read" || ref.Source != store.SourceSession {
		t.Fatalf("persisted capture: %+v", ref)
	}
}
