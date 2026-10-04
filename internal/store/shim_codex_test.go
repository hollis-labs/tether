//go:build !windows

package store

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/substrate/harness/shim"
	"path/filepath"
	"testing"

	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/shimcodex"
)

func TestCodexProtocolCASBindsCanonicalGenerationAndAtomicInbox(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err = db.CreateSession(SessionRow{ID: "codex", State: "running"}, &launch.Plan{ProviderBrand: "codex"}); err != nil {
		t.Fatal(err)
	}
	row := SessionShimRow{SessionID: "codex", ShimKey: "key", HostBackend: "detached", SocketPath: "/private/c", DescriptorPath: "/private/launch.json", Runtime: "codex", RuntimeGeneration: 1, BootGeneration: "boot", JournalID: "j"}
	if err = db.UpsertSessionShim(ctx, row); err != nil {
		t.Fatal(err)
	}
	p, err := db.CodexProtocolStore(ctx, "codex", "key", 1024)
	if err != nil {
		t.Fatal(err)
	}
	state := shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: "codex", Instance: "i", Operation: "key", Generation: 1, Journal: "j", Attempt: "a", Fingerprint: "f"}, Revision: 1, Epoch: 1, NextID: shimcodex.FirstID}
	if err = p.Commit(ctx, 0, state); err != nil {
		t.Fatal(err)
	}
	state.Revision = 2
	state.Cursor = "j:1"
	state.Inbox = []shimcodex.Event{{Identity: "j:event:1", Cursor: "j:1", Raw: []byte(`{"method":"fixture"}`)}}
	if err = p.Commit(ctx, 1, state); err != nil {
		t.Fatal(err)
	}
	loaded, err := p.Load(ctx)
	if err != nil || loaded.Cursor != "j:1" || len(loaded.Inbox) != 1 {
		t.Fatalf("cursor/inbox split: %+v %v", loaded, err)
	}
	stale := state
	stale.Cursor = "j:2"
	stale.Inbox = nil
	if err = p.Commit(ctx, 1, stale); !errors.Is(err, ErrSessionShimConflict) {
		t.Fatalf("stale CAS erased inbox: %v", err)
	}
	foreign := state
	foreign.Revision = 3
	foreign.Binding.Generation++
	if err = p.Commit(ctx, 2, foreign); !errors.Is(err, ErrSessionShimConflict) {
		t.Fatalf("foreign generation committed: %v", err)
	}
	loaded, err = p.Load(ctx)
	if err != nil || loaded.Cursor != "j:1" || len(loaded.Inbox) != 1 {
		t.Fatal("failed update mutated accepted output")
	}
	if _, err = db.db.ExecContext(ctx, `UPDATE session_shims SET runtime_generation='2' WHERE session_id='codex'`); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Load(ctx); !errors.Is(err, ErrSessionShimConflict) {
		t.Fatalf("stale controller borrowed replacement generation: %v", err)
	}
	state.Revision = 3
	if err = p.Commit(ctx, 2, state); !errors.Is(err, ErrSessionShimConflict) {
		t.Fatalf("stale controller modified replacement generation: %v", err)
	}
}

// This models a private opaque verifier token, NOT a B2 issuer/transaction.
// Production LoadVerifiedCodexDelivery remains Unsupported; there is no public
// constructor and no successful receipt producer in this source slice.
func TestVerifiedCodexDeliveryConsumerBindsOpaqueIssuerAndObservation(t *testing.T) {
	db := &Store{}
	state := shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: "s", Instance: "i", Operation: "p", Attempt: "a", Fingerprint: "f", Generation: 1, Journal: "j"}, Revision: 5, Epoch: 2, NextID: shimcodex.FirstID, Cursor: "j:5", ReplayHighWater: "j:3", ExitCursor: "j:4", Exit: &shim.Exit{Status: 7}}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	proof := &VerifiedCodexDelivery{issuer: db, receiptID: "private-model", observationDigest: sha256.Sum256(raw), acceptedCursor: "j:5", deliveredCursor: "j:5", replayHighWater: "j:3", terminalIdentity: "j:exit:j:4", terminal: *state.Exit}
	if err = proof.Validate(db, state, "j:3", true); err != nil {
		t.Fatal("bound verifier rejected model", err)
	}
	if _, err = db.LoadVerifiedCodexDelivery(context.Background(), state, "j:3"); !errors.Is(err, ErrCodexDeliveryUnsupported) {
		t.Fatal("production minted an unearned receipt")
	}
	for _, kind := range []string{"issuer", "revision", "epoch", "attempt", "fingerprint", "journal", "carry", "input", "terminal", "highwater", "terminal_cursor", "zero"} {
		t.Run(kind, func(t *testing.T) {
			changed := state
			issuer := db
			candidate := *proof
			high := "j:3"
			switch kind {
			case "issuer":
				issuer = &Store{}
			case "revision":
				changed.Revision++
			case "epoch":
				changed.Epoch++
			case "attempt":
				changed.Binding.Attempt = "replacement"
			case "fingerprint":
				changed.Binding.Fingerprint = "replacement"
			case "journal":
				changed.Binding.Journal = "other"
			case "carry":
				changed.Partial = []byte("{")
			case "input":
				changed.Operations = []shimcodex.Operation{{Phase: shimcodex.Attempted}}
			case "terminal":
				candidate.terminalIdentity = "j:exit:j:other"
			case "highwater":
				high = "j:6"
				changed.ReplayHighWater = high
				candidate.replayHighWater = high
				raw, _ := json.Marshal(changed)
				candidate.observationDigest = sha256.Sum256(raw)
			case "terminal_cursor":
				changed.ExitCursor = "j:6"
				candidate.terminalIdentity = "j:exit:j:6"
				raw, _ := json.Marshal(changed)
				candidate.observationDigest = sha256.Sum256(raw)
			case "zero":
				candidate = VerifiedCodexDelivery{}
			}
			if err := candidate.Validate(issuer, changed, high, true); !errors.Is(err, ErrCodexDeliveryUnsupported) {
				t.Fatalf("stale/forged model accepted: %v", err)
			}
		})
	}
}
