package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/store"
)

func TestBootResumePendingMailWithoutCheckpointLeavesMailUnread(t *testing.T) {
	r := newCodexRig(t)
	id := r.start()
	if err := r.turn(id, "initial work"); err != nil {
		t.Fatal(err)
	}
	r.wait(1)
	if err := r.svc.StopSession(id); err != nil {
		t.Fatal(err)
	}
	r.ended(id)
	from, _ := messaging.ParseURN("msg://agent/local/sender")
	to, _ := messaging.ParseURN(registry.LogicalAgentBindingTarget("agent"))
	sent, err := r.svc.Store.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: json.RawMessage(`{"body":"pending follow-up"}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.svc.BootResumeSessions(context.Background())
	recovery := r.wait(2)
	if !strings.Contains(recovery.argv[len(recovery.argv)-1], "pending follow-up") {
		t.Fatal("unread intent missing from recovery pack")
	}
	row, err := r.svc.Store.LatestSessionForAgent(context.Background(), "agent")
	if err != nil || row.ID == id || row.State != "running" {
		t.Fatalf("boot recovery=%+v %v", row, err)
	}
	page, err := r.svc.Store.MessagingStore().List(context.Background(), to, store.ListFilter{UnreadOnly: true, Limit: 20})
	if err != nil || page.Total != 1 || page.Messages[0].ID != sent.ID {
		t.Fatalf("recovery consumed mail=%+v %v", page, err)
	}
	r.idle(row.ID)
	r.svc.BootResumeSessions(context.Background())
	if r.count() != 2 {
		t.Fatal("boot recovery duplicated a live agent")
	}
}

// pendingMailRig returns a rig whose agent has ended and holds one unread
// message, so ordinary selection would resume it.
func pendingMailRig(t *testing.T) (*codexRig, messaging.Address) {
	t.Helper()
	r := newCodexRig(t)
	id := r.start()
	if err := r.turn(id, "initial work"); err != nil {
		t.Fatal(err)
	}
	r.wait(1)
	if err := r.svc.StopSession(id); err != nil {
		t.Fatal(err)
	}
	r.ended(id)
	from, _ := messaging.ParseURN("msg://agent/local/sender")
	to, _ := messaging.ParseURN(registry.LogicalAgentBindingTarget("agent"))
	if _, err := r.svc.Store.MessagingStore().Send(context.Background(), messaging.Envelope{From: from, To: to, Kind: messaging.MsgKindNotice, Payload: json.RawMessage(`{"body":"pending"}`)}); err != nil {
		t.Fatal(err)
	}
	return r, to
}

func requireStillCold(t *testing.T, r *codexRig, to messaging.Address) {
	t.Helper()
	if r.count() != 1 {
		t.Fatalf("agent was resumed: starts=%d", r.count())
	}
	page, err := r.svc.Store.MessagingStore().List(context.Background(), to, store.ListFilter{UnreadOnly: true, Limit: 20})
	if err != nil || page.Total != 1 {
		t.Fatalf("mail consumed=%+v %v", page, err)
	}
}

func TestBootResumeDefaultNeverSkipsAndRecordsDecision(t *testing.T) {
	for _, recovery := range []string{"", "never"} {
		r, to := pendingMailRig(t)
		r.svc.Catalog.Agents["agent"] = config.Agent{ID: "agent", Recovery: recovery}
		r.svc.BootResumeSessions(context.Background())
		requireStillCold(t, r, to)
		var found bool
		for _, e := range r.bus.got {
			if e.Kind == "session.boot_recovery" && strings.Contains(e.PayloadJSON, `"skipped"`) && strings.Contains(e.PayloadJSON, "recovery_not_auto") {
				found = true
			}
		}
		if !found {
			t.Fatalf("recovery=%q: no skipped session.boot_recovery event", recovery)
		}
	}
}

func TestBootResumeAutoResumes(t *testing.T) {
	r, _ := pendingMailRig(t)
	r.svc.BootResumeSessions(context.Background())
	r.wait(2)
}

func TestBootResumeReadinessGateSkipsEverything(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, root := range []string{missing, file} {
		r, to := pendingMailRig(t)
		d := &r.svc.Catalog.Global.Catalog.Defaults
		d.WorkspaceRoot = root
		before := len(r.bus.kinds())
		teamCalled := false
		r.svc.BootTeamRecovery = func(context.Context) error { teamCalled = true; return nil }
		r.svc.BootResumeSessions(context.Background())
		requireStillCold(t, r, to)
		if teamCalled || len(r.bus.got) != before {
			t.Fatalf("root %q: gate had side effects (team=%v)", root, teamCalled)
		}
	}
}

func TestBootResumeIdleAgentStaysCold(t *testing.T) {
	r := newCodexRig(t)
	id := r.start()
	if err := r.svc.StopSession(id); err != nil {
		t.Fatal(err)
	}
	r.ended(id)
	r.svc.BootResumeSessions(context.Background())
	row, err := r.svc.Store.LatestSessionForAgent(context.Background(), "agent")
	if err != nil || row.ID != id || r.count() != 0 {
		t.Fatalf("idle agent started=%+v %v", row, err)
	}
}

func TestRecoveryReadRefusalDoesNotBecomeAssignment(t *testing.T) {
	r := newCodexRig(t)
	r.svc.RecoveryReadTool = func(_ context.Context, _, _, tool string, _ map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"ok":false,"data":{"items":[{"id":"denied","status":"doing"}]}}`), nil
	}
	pack := r.svc.recoveryContext(context.Background(), &launch.Plan{LogicalAgentID: "agent", WorkRoot: r.repo}, nil)
	if pack.openAssignment || len(pack.Tasks) > 0 || len(pack.Omissions) == 0 {
		t.Fatalf("refusal became work=%+v", pack)
	}
}

func TestRecoveryContextBoundDoesNotAdvanceOmittedChannels(t *testing.T) {
	cursors := map[string]int64{"channel": 9}
	p := recoveryPack{Version: "v0", cursors: cursors, Channels: []recoveryChannel{{Name: strings.Repeat("x", 70*1024)}}}
	prompt := p.prompt("")
	if len(cursors) != 0 || !strings.Contains(prompt, "omitted") || len(prompt) > 65*1024 {
		t.Fatalf("omitted context advanced cursors: %v prompt bytes=%d", cursors, len(prompt))
	}
}
