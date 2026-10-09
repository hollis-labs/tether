//go:build !windows

package store

import (
	"context"
	"encoding/json"
	"errors"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/registry"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"path/filepath"
	"testing"
	"time"
)

func codexDeliverySQLFixture(t *testing.T) (*Store, shimcodex.State) {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx := context.Background()
	if err = db.CreateSession(SessionRow{ID: "s", State: "running"}, &launch.Plan{ProviderBrand: "codex"}); err != nil {
		t.Fatal(err)
	}
	row := SessionShimRow{SessionID: "s", ShimKey: "p", HostBackend: "detached", SocketPath: "/private/c", DescriptorPath: "/private/d", Runtime: "codex", RuntimeGeneration: 1, BootGeneration: "boot", JournalID: "j"}
	if err = db.UpsertSessionShim(ctx, row); err != nil {
		t.Fatal(err)
	}
	p, err := db.CodexProtocolStore(ctx, "s", "p", shimcodex.ProjectionBudget)
	if err != nil {
		t.Fatal(err)
	}
	state := shimcodex.State{Version: shimcodex.Version, Binding: shimcodex.Binding{Session: "s", Instance: "i", Generation: 1, Operation: "p", Attempt: "a", Fingerprint: "f", Journal: "j"}, Revision: 1, Epoch: 1, NextID: shimcodex.FirstID, Cursor: "j:1", Inbox: []shimcodex.Event{{Identity: "j:j:1:shim.attached", Cursor: "j:1", Raw: []byte(`{"kind":"shim.attached","payload":{}}`)}}}
	if err = p.Commit(ctx, 0, state); err != nil {
		t.Fatal(err)
	}
	return db, state
}
func frozenCodexStage() messaging.Envelope {
	return messaging.Envelope{ID: "source-output", Kind: messaging.MsgKindNotice, From: messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "s"}, To: messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "turn-output"}, ThreadID: "s", CreatedAt: time.Unix(1700000000, 0).UTC(), ContentType: "text/plain", Payload: []byte("hello"), Metadata: map[string]string{"session_id": "s", "codex_source_id": "j:source:1"}}
}
func TestCodexCandidateSameTransactionReadStageRollbackAndRefusal(t *testing.T) {
	db, state := codexDeliverySQLFixture(t)
	db.db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	credentials := identity.NewStore(db.db)
	token, err := credentials.Mint(ctx, identity.Principal{ID: "principal", Kind: "session", SessionID: "s"})
	if err != nil {
		t.Fatal(err)
	}
	bindings := registry.NewStorage(db.db)
	first, err := bindings.LeaseBinding(ctx, "msg://session/local/s", "s", "host", "attempt", nil, registry.VisibilityPrivateLocal, 0)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	snapshot, err := readCodexCandidateTx(ctx, tx, state)
	if err != nil || len(snapshot.Protocol.Inbox) != 1 {
		t.Fatal("same-tx source read", err)
	}
	if _, err = identity.VerifyTx(ctx, tx, token); err != nil {
		t.Fatal("same-tx credential read", err)
	}
	current, err := registry.CurrentBindingTx(ctx, tx, first.TargetURN)
	if err != nil || current.ID != first.ID {
		t.Fatal("same-tx live binding read", err)
	}
	env := frozenCodexStage()
	if _, err = stageCodexSourceTx(ctx, tx, env); err != nil {
		t.Fatal(err)
	}
	if _, err = stageCodexSourceTx(ctx, tx, env); err != nil {
		t.Fatal("same source not idempotent", err)
	}
	changedTime := env
	changedTime.CreatedAt = env.CreatedAt.Add(time.Second)
	if _, err = stageCodexSourceTx(ctx, tx, changedTime); err == nil {
		t.Fatal("changed frozen source timestamp accepted")
	}
	env.Payload = []byte("splice")
	if _, err = stageCodexSourceTx(ctx, tx, env); err == nil {
		t.Fatal("changed source payload accepted")
	}
	if _, err = tx.ExecContext(ctx, `UPDATE principals SET revoked_at=? WHERE principal_id=?`, time.Now().UTC().Format(time.RFC3339Nano), "principal"); err != nil {
		t.Fatal(err)
	}
	if _, err = identity.VerifyTx(ctx, tx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("transaction ignored revoke", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_bindings SET revoked_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CurrentBindingTx(ctx, tx, first.TargetURN); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatal("transaction ignored binding revoke", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE principals SET revoked_at=NULL,expires_at=? WHERE principal_id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), "principal"); err != nil {
		t.Fatal(err)
	}
	if _, err = identity.VerifyTx(ctx, tx, token); !errors.Is(err, identity.ErrInvalidToken) {
		t.Fatal("transaction ignored principal expiry", err)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE runtime_bindings SET revoked_at=NULL,lease_expires_at=? WHERE id=?`, time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano), first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = registry.CurrentBindingTx(ctx, tx, first.TargetURN); !errors.Is(err, registry.ErrBindingNotFound) {
		t.Fatal("transaction ignored binding expiry", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var count int
	if err = db.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id='source-output'`).Scan(&count); err != nil || count != 0 {
		t.Fatal("staging escaped rollback", count, err)
	}
	if _, err = credentials.Verify(ctx, token); err != nil {
		t.Fatal("read adapter mutated credential", err)
	}
	if _, err = bindings.CurrentBinding(ctx, first.TargetURN); err != nil {
		t.Fatal("read adapter mutated binding", err)
	}
	loaded, err := db.ReadCodexCandidate(ctx, state)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(state)
	b, _ := json.Marshal(loaded.Protocol)
	if string(a) != string(b) {
		t.Fatal("candidate consumed inbox")
	}
	if proof, err := db.AcceptCodexDelivery(ctx, shimcodex.Projection{}); proof != nil || !errors.Is(err, ErrCodexDeliveryUnsupported) {
		t.Fatal("fixture manufactured successful issuer")
	}
}
func TestCodexCandidateRejectsSameRevisionDifferentObservation(t *testing.T) {
	db, state := codexDeliverySQLFixture(t)
	state.Inbox = nil
	if _, err := db.ReadCodexCandidate(context.Background(), state); !errors.Is(err, ErrSessionShimConflict) {
		t.Fatal("same revision borrowed different inbox", err)
	}
}

func TestCodexStagingCommitIsOnlyOutboxAcceptanceAndNeverReceipt(t *testing.T) {
	db, state := codexDeliverySQLFixture(t)
	ctx := context.Background()
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	env := frozenCodexStage()
	saved, err := stageCodexSourceTx(ctx, tx, env)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	got, err := db.StagedTurnOutput(ctx, saved.ID)
	if err != nil || string(got.Payload) != "hello" {
		t.Fatal("lost staged commit", err)
	}
	loaded, err := db.ReadCodexCandidate(ctx, state)
	if err != nil || len(loaded.Protocol.Inbox) != 1 {
		t.Fatal("stage cleared inbox", err)
	}
	if proof, err := db.LoadVerifiedCodexDelivery(ctx, state, "j:1"); proof != nil || !errors.Is(err, ErrCodexDeliveryUnsupported) {
		t.Fatal("stage fabricated receipt")
	}
	tx, err = db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	env.Metadata["codex_source_id"] = "replacement"
	if _, err = stageCodexSourceTx(ctx, tx, env); err == nil {
		t.Fatal("same payload borrowed other source identity")
	}
}
