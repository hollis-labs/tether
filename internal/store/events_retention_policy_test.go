package store

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/hollis-labs/tether/internal/eventretention"
	"github.com/hollis-labs/tether/internal/events"
)

func installTestRetention(t *testing.T, s *Store, raw string, kinds map[string]eventretention.Class) eventretention.Config {
	t.Helper()
	cfg, _, err := eventretention.Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetEventRetentionAdmission(context.Background(), cfg, kinds); err != nil {
		t.Fatal(err)
	}
	return cfg
}
func TestRetentionAdmissionIsTrustedAndSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	first := installTestRetention(t, db, "", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	e := fullEnvelope()
	e.Envelope.RetentionClass = events.RetentionDebug
	e.Envelope.RetentionPolicyID = "forged"
	stored, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	row := rowByID(t, db, stored.Seq)
	if row["retention_class"] != events.RetentionState || row["retention_policy_id"] != first.ID() || row["payload_digest"] != nil {
		t.Fatal("producer selected policy or digest at insert")
	}
	e.Kind = "unknown.kind"
	unknown, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	row = rowByID(t, db, unknown.Seq)
	if row["retention_class"] != nil || row["retention_policy_id"] != nil {
		t.Fatal("unknown kind classified")
	}
	second := installTestRetention(t, db, "retention: {classes: {state: {full: 1h}}}", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	row = rowByID(t, db, stored.Seq)
	if row["retention_policy_id"] != first.ID() {
		t.Fatal("policy change retroactively changed row")
	}
	// A second connection uses the durable catalog without process-local hooks.
	reopened, err := Open(db.stateDBPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	newer, err := reopened.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	if rowByID(t, db, newer.Seq)["retention_policy_id"] != second.ID() {
		t.Fatal("admission policy not durable")
	}
	if _, err := db.db.Exec(`UPDATE event_retention_policies SET policy_json='{}' WHERE id=?`, first.ID()); err == nil {
		t.Fatal("policy snapshot mutable")
	}
}
func TestRetentionTombstonesExactPayloadAndPreservesAllOtherColumns(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	installTestRetention(t, db, "", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	ids := []int64{}
	before := map[int64]map[string]any{}
	for _, pii := range []bool{false, true} {
		e := fullEnvelope()
		e.Envelope.Class.PII = pii
		e.PayloadJSON = `{ "value":  1 }`
		stored, err := db.InsertEventEnvelope(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, stored.Seq)
		before[stored.Seq] = rowByID(t, db, stored.Seq)
	}
	now := time.Now().Add(181 * eventretention.Day)
	batch, err := db.TombstoneEventPayloads(ctx, now, 0, 1)
	if err != nil || batch.Scanned != 1 || batch.Tombstoned != 1 || batch.NextCursor != ids[0] {
		t.Fatalf("bounded batch %+v %v", batch, err)
	}
	batch, err = db.TombstoneEventPayloads(ctx, now, batch.NextCursor, 1)
	if err != nil || batch.Tombstoned != 1 {
		t.Fatalf("second %+v %v", batch, err)
	}
	for i, id := range ids {
		after := rowByID(t, db, id)
		if after["payload_json"] != nil {
			t.Fatal("payload not removed")
		}
		want := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(`{ "value":  1 }`)))
		if i == 1 {
			want = "withheld"
		}
		if after["payload_digest"] != want {
			t.Fatalf("digest %v want %s", after["payload_digest"], want)
		}
		delete(after, "payload_json")
		delete(after, "payload_digest")
		delete(before[id], "payload_json")
		delete(before[id], "payload_digest")
		if !reflect.DeepEqual(before[id], after) {
			t.Fatal("tombstone changed envelope/cursor metadata")
		}
	}
	batch, err = db.TombstoneEventPayloads(ctx, now, 0, 100)
	if err != nil || batch.Tombstoned != 0 || batch.Scanned != 0 {
		t.Fatalf("repeat %+v %v", batch, err)
	}
}
func TestRetentionPendingLegacyAndYoungPayloadsStayIntact(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	legacy, err := db.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	installTestRetention(t, db, "retention: {classes: {state: {full: 1h}}}", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	young, err := db.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.InsertEventTx(ctx, tx, fullEnvelope())
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	batch, err := db.TombstoneEventPayloads(ctx, time.Now(), 0, 100)
	if err != nil || batch.Tombstoned != 0 {
		t.Fatalf("young %+v %v", batch, err)
	}
	batch, err = db.TombstoneEventPayloads(ctx, time.Now().Add(2*time.Hour), 0, 100)
	if err != nil || batch.Tombstoned != 1 {
		t.Fatalf("eligible %+v %v", batch, err)
	}
	if rowByID(t, db, young.Seq)["payload_json"] != nil {
		t.Fatal("eligible payload retained")
	}
	for _, id := range []int64{legacy.Seq, pending.Seq} {
		if rowByID(t, db, id)["payload_json"] == nil {
			t.Fatal("legacy/pending payload touched")
		}
	}
	if err := db.MarkPublishedContext(ctx, []int64{pending.Seq}); err != nil {
		t.Fatal(err)
	}
	batch, err = db.TombstoneEventPayloads(ctx, time.Now().Add(2*time.Hour), 0, 100)
	if err != nil || batch.Tombstoned != 1 {
		t.Fatalf("published retry %+v %v", batch, err)
	}
}
func TestRetentionTombstoneFailureRollsBackWholeBatch(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	installTestRetention(t, db, "retention: {classes: {state: {full: 1h}}}", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	first, err := db.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	second, err := db.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(fmt.Sprintf(`CREATE TRIGGER reject_second_tombstone BEFORE UPDATE OF payload_digest ON events WHEN NEW.id=%d BEGIN SELECT RAISE(ABORT,'injected tombstone failure'); END`, second.Seq)); err != nil {
		t.Fatal(err)
	}
	before := rowByID(t, db, first.Seq)
	batch, err := db.TombstoneEventPayloads(ctx, time.Now().Add(2*time.Hour), 0, 100)
	if err == nil || batch != (EventTombstoneBatch{}) {
		t.Fatalf("failed batch reported success: %+v %v", batch, err)
	}
	if !reflect.DeepEqual(before, rowByID(t, db, first.Seq)) {
		t.Fatal("earlier update committed despite later error")
	}
}
func TestRetentionEnvironmentRequestPreservesPIIAndRefusesSecrets(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	installTestRetention(t, db, "", map[string]eventretention.Class{EnvironmentRequestKind: eventretention.Audit})
	e := fullEnvelope()
	e.Kind = EnvironmentRequestKind
	e.PayloadJSON = `{"turn_id":"turn-1","request_id":"req-1","request_kind":"question","open":true}`
	stored, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	row := rowByID(t, db, stored.Seq)
	if row["pii"] != int64(1) || row["retention_class"] != events.RetentionAudit {
		t.Fatal("special path lost sensitivity/class")
	}
	batch, err := db.TombstoneEventPayloads(ctx, time.Now().Add(31*eventretention.Day), 0, 100)
	if err != nil || batch.Tombstoned != 1 || rowByID(t, db, stored.Seq)["payload_digest"] != "withheld" {
		t.Fatalf("PII audit cap %+v %v", batch, err)
	}
	for _, kind := range []string{EnvironmentRequestKind, "session.state_changed"} {
		e.Kind = kind
		e.Envelope.Class.Sensitivity = "secret"
		if _, err := db.InsertEventEnvelope(ctx, e); err == nil {
			t.Fatal("secret stored")
		}
	}
}

func TestRetentionAdmissionUpdateAndOutboxRollbackAreAtomic(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	first := installTestRetention(t, db, "", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	second, _, err := eventretention.Parse([]byte("retention: {classes: {state: {full: 1h}}}"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER reject_catalog BEFORE INSERT ON event_retention_admission WHEN NEW.kind='reject' BEGIN SELECT RAISE(ABORT,'injected catalog failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := db.SetEventRetentionAdmission(ctx, second, map[string]eventretention.Class{"reject": eventretention.Debug}); err == nil {
		t.Fatal("catalog failure ignored")
	}
	stored, err := db.InsertEventEnvelope(ctx, fullEnvelope())
	if err != nil {
		t.Fatal(err)
	}
	if rowByID(t, db, stored.Seq)["retention_policy_id"] != first.ID() {
		t.Fatal("failed policy update changed admission")
	}
	var n int
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM event_retention_policies WHERE id=?`, second.ID()).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed update persisted snapshot")
	}
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := db.InsertEventTx(ctx, tx, fullEnvelope())
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := db.db.QueryRow(`SELECT COUNT(*) FROM events WHERE event_id=?`, pending.EventID).Scan(&n); err != nil || n != 0 {
		t.Fatal("rolled-back outbox admission persisted")
	}
}

func TestRetentionUsesAdmissionPolicyAfterConfigChangeAndRejectsMissingPolicy(t *testing.T) {
	ctx := context.Background()
	db := openEventsStore(t)
	installTestRetention(t, db, "", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	e := fullEnvelope()
	e.Envelope.Class.PII = false
	old, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	installTestRetention(t, db, "retention: {classes: {state: {full: 1h}}}", map[string]eventretention.Class{"session.state_changed": eventretention.State})
	recent, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	batch, err := db.TombstoneEventPayloads(ctx, time.Now().Add(2*time.Hour), 0, 100)
	if err != nil || batch.Tombstoned != 1 {
		t.Fatalf("forward policy %+v %v", batch, err)
	}
	if rowByID(t, db, old.Seq)["payload_json"] == nil || rowByID(t, db, recent.Seq)["payload_json"] != nil {
		t.Fatal("new policy applied retroactively")
	}
	corrupt, err := db.InsertEventEnvelope(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`UPDATE events SET retention_policy_id='missing-policy' WHERE id=?`, corrupt.Seq); err != nil {
		t.Fatal(err)
	}
	before := rowByID(t, db, old.Seq)
	batch, err = db.TombstoneEventPayloads(ctx, time.Now().Add(181*eventretention.Day), 0, 100)
	if err == nil || batch != (EventTombstoneBatch{}) || !reflect.DeepEqual(before, rowByID(t, db, old.Seq)) {
		t.Fatal("missing policy did not fail closed/rollback")
	}
}
