//go:build linux

package app

import (
	"context"
	"encoding/json"
	"testing"

	gop "github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/tether/internal/events"
)

func TestShimResultReplayUsesPublishedIdentity(t *testing.T) {
	f := shimFixture(t)
	payload, _ := json.Marshal(events.TurnOutputEvent{SessionID: f.req.ID, TurnID: "first", ProviderResultID: "already-published"})
	if err := f.svc.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: f.req.ID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(payload)}); err != nil {
		t.Fatal(err)
	}
	adapter := &shimClaudeAdapter{ClaudeAdapter: gop.NewClaudeAdapterStreamingStdio(), service: f.svc, sessionID: f.req.ID}
	for _, tc := range []struct {
		uuid      string
		duplicate bool
	}{{"already-published", true}, {"new-identity", false}, {"", false}} {
		line, _ := json.Marshal(map[string]string{"type": "result", "subtype": "success", "uuid": tc.uuid, "result": "same text"})
		legacy, err := adapter.ParseLine(line)
		if err != nil {
			t.Fatal(err)
		}
		typed, err := adapter.ParseLineEvents(line)
		if err != nil {
			t.Fatal(err)
		}
		if tc.duplicate && (len(legacy) != 0 || len(typed) != 0) {
			t.Fatal("published result replayed")
		}
		if !tc.duplicate && len(typed) == 0 {
			t.Fatal("identity-free or different result suppressed by text")
		}
	}
}
