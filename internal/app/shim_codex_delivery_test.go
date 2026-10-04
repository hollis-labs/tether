//go:build !windows

package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/store"
)

func TestHostedCodexPureCandidateRetainsInboxAndCannotIssueDelivery(t *testing.T) {
	s, _, p := codexOutputFixture(t)
	updateCodexOutputFixture(t, p, func(state *shimcodex.State) {
		state.Cursor = "j:1"
		state.Inbox = []shimcodex.Event{{Identity: "j:j:1:shim.attached", Cursor: "j:1", Raw: []byte(`{"kind":"shim.attached","payload":{}}`)}}
	})
	state, err := p.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	previous := shimcodex.Projection{Version: shimcodex.ProjectionVersion, Binding: state.Binding, ProtocolRevision: state.Revision, JournalIdentity: state.Binding.Journal}
	candidate, err := s.codexDeliveryCandidate(context.Background(), state, previous)
	if !errors.Is(err, store.ErrCodexDeliveryUnsupported) || len(candidate.Sources) != 1 || candidate.Sources[0].Disposition != "metadata_only" || candidate.DeliveredHighWater != "" || candidate.Terminal != nil {
		t.Fatal("candidate manufactured receipt", err)
	}
	after, err := p.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(state)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("candidate consumed or rewrote protocol")
	}
	if proof, err := s.Store.AcceptCodexDelivery(context.Background(), candidate); proof != nil || !errors.Is(err, store.ErrCodexDeliveryUnsupported) {
		t.Fatal("candidate became production success")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.codexDeliveryCandidate(cancelled, state, previous); err == nil {
		t.Fatal("cancelled candidate succeeded")
	}
}
