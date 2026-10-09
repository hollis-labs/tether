package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	messaging "github.com/hollis-labs/substrate/mesh/messaging"
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
