package app

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/api"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

func TestNativeOnlyMissingRecordedContextRefusesBeforeAllocation(t *testing.T) {
	r := newCodexRig(t)
	const id = "canonical-source"
	plan := &launch.Plan{ProviderBrand: "codex", ProviderID: "codex-cli", WorkRoot: filepath.Join(t.TempDir(), "missing-repository"), NativeStateRoot: t.TempDir()}
	if err := r.svc.Store.CreateSession(store.SessionRow{ID: id, LaunchID: "codex-launch", LogicalAgentID: "agent", ProviderID: "codex-cli", Workspace: t.TempDir(), State: "failed"}, plan); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.UpsertSessionProviderMapping(id, "tether", "codex-cli", "retained-thread"); err != nil {
		t.Fatal(err)
	}
	if err := r.svc.Store.SetLogicalAgentLaunchID("agent", "codex-launch"); err != nil {
		t.Fatal(err)
	}
	result, err := r.svc.ResumeLogicalAgentWithContext(context.Background(), "agent", api.ResumeOptions{NativeOnly: true, SourceSessionID: id})
	if !errors.Is(err, ErrNativeOnlyUnavailable) || result.SessionID != "" {
		t.Fatalf("missing context admitted: %+v %v", result, err)
	}
	latest, err := r.svc.Store.LatestSessionForAgent(context.Background(), "agent")
	if err != nil || latest.ID != id || r.count() != 0 {
		t.Fatal("missing context allocated or launched destination", err)
	}
	mapping, err := r.svc.Store.GetSessionProviderMapping(id, "tether", "codex-cli")
	if err != nil || mapping.NativeSessionID.String != "retained-thread" {
		t.Fatal("source mapping changed", err)
	}
}
