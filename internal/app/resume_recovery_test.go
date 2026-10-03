package app

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/tether/internal/store"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/checkpoint"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/provider/api/stub"
	"github.com/hollis-labs/tether/internal/session"
)

func TestResumeRecoveryStates(t *testing.T) {
	for _, state := range []string{"detached", "orphaned"} {
		t.Run(state, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			svc := bindingHarness(t)
			svc.CatalogRoot = t.TempDir()
			svc.Catalog = &config.Catalog{
				Global:    config.Global{Version: "test"},
				Projects:  map[string]config.Project{"p": {ID: "p", RepoRoot: t.TempDir(), Workspace: config.WorkspaceSpec{SessionRoot: t.TempDir(), DefaultMode: "shared"}}},
				Agents:    map[string]config.Agent{"worker": {ID: "worker"}},
				Providers: map[string]config.Provider{"stub": {ID: "stub", Provider: "claude", RuntimeKind: config.RuntimeKindPTY, Command: "test-fake"}},
				Launches:  map[string]config.Launch{"l1": {ID: "l1", Project: "p", Agent: "worker", Provider: "stub"}},
			}
			svc.factories = map[string]RuntimeFactory{"stub": stub.New}
			runningSession(t, svc, "parent", "fake", 123, "")
			if err := svc.Store.SetLogicalAgentLaunchID("worker", "l1"); err != nil {
				t.Fatal(err)
			}
			if err := svc.Store.CreateCheckpoint(checkpoint.Checkpoint{ID: "checkpoint", LogicalAgentID: "worker", SourceSessionID: "parent", Summary: "resume context", CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
				t.Fatal(err)
			}
			if state == "detached" {
				if err := svc.MarkSessionDetached("parent", "daemon-shutdown"); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := svc.MarkSessionOrphaned("parent", "shim_gone"); err != nil {
					t.Fatal(err)
				}
			}
			res, err := svc.ResumeLogicalAgent("worker", api.ResumeOptions{})
			if state == "detached" {
				if !errors.Is(err, session.ErrDetached) {
					t.Fatalf("resume = %+v %v; want ErrDetached", res, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), res.SessionID) })
			row, err := svc.Store.GetSession(res.SessionID)
			if err != nil || row.ParentSessionID.String != "parent" || row.Intent != "resume" || row.State != "running" {
				t.Fatalf("resumed row = %+v %v", row, err)
			}
			parent, _ := svc.Store.GetSession("parent")
			if parent.State != "orphaned" {
				t.Fatalf("resume mutated parent to %s", parent.State)
			}
		})
	}
}

// recoveryResumeAPI runs the real resume implementation behind its HTTP handler.
type recoveryResumeAPI struct {
	api.LaunchService
	svc *Service
}

func (a recoveryResumeAPI) ResumeLogicalAgent(id string, opts api.ResumeOptions) (api.LaunchResult, error) {
	return a.svc.ResumeLogicalAgent(id, opts)
}

func TestResumeRefusesDetachedSiblingWithBinding(t *testing.T) {
	svc := bindingHarness(t)
	runningSession(t, svc, "checkpoint-source", "fake", 111, "")
	if err := svc.MarkSessionOrphaned("checkpoint-source", "shim_gone"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.SetLogicalAgentLaunchID("worker", "l1"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Store.CreateCheckpoint(checkpoint.Checkpoint{ID: "checkpoint", LogicalAgentID: "worker", SourceSessionID: "checkpoint-source", CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	runningSession(t, svc, "alive-sibling", "fake", 222, "")
	svc.leaseActorBinding("alive-sibling", "worker")
	if err := svc.MarkSessionDetached("alive-sibling", "daemon-shutdown"); err != nil {
		t.Fatal(err)
	}
	before, err := svc.Store.ListSessions(store.ListSessionsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	api.NewHandler(api.Deps{Service: recoveryResumeAPI{svc: svc}, Checkpoints: svc.Store}).ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/logical-agents/worker/resume", nil))
	if rr.Code != http.StatusConflict {
		t.Fatalf("response = %d %s", rr.Code, rr.Body)
	}
	var response struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Code != api.CodeConflict {
		t.Fatalf("error = %s", rr.Body)
	}
	after, err := svc.Store.ListSessions(store.ListSessionsOptions{})
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("refused resume changed sessions: %v", err)
	}
	if b, err := currentWorkerBinding(svc); err != nil || b.SessionID != "alive-sibling" {
		t.Fatalf("lost sibling lease: %+v %v", b, err)
	}
}
