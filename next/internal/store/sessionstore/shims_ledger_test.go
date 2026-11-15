package sessionstore_test

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"strings"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/sessionstore"
	"github.com/hollis-labs/tether/next/internal/store/storetest"
)

// The old store's accounting snapshots (internal/store/codex_replacement_accounting.go at main
// f6c37bfa: accountingPublicSnapshot, accountingMessagingSnapshot, accountingHashPart), transcribed
// verbatim: SQL text, scan into strings, JSON row framing, budgets. The persisted replacement
// custody evidence carries these digests, so the port must reproduce them byte for byte.

const oldOutboxPredicate = `from_urn=? AND (to_urn='msg://service/local/turn-output' OR CASE WHEN json_valid(metadata) THEN json_extract(metadata,'$.output_id') END IS NOT NULL)`

const oldDeliveryPredicate = `d.recipient_urn=? OR CASE WHEN json_valid(d.binding_json) THEN json_extract(d.binding_json,'$.session_id') END=? OR EXISTS(SELECT 1 FROM messaging_attempts prior WHERE prior.delivery_id=d.id AND (prior.holder=? OR CASE WHEN json_valid(prior.binding_json) THEN json_extract(prior.binding_json,'$.session_id') END=?))`

var errOldRefused = errors.New("old accounting refused")

func oldHashPart(h hash.Hash, part []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(part)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(part)
}

type oldQuery struct {
	sql  string
	args []any
}

func oldDigest(ctx context.Context, q gostore.Querier, queries []oldQuery, rowLimit int, check func(raw []byte, total int) bool) (string, error) {
	h := sha256.New()
	bytesRead := 0
	for _, query := range queries {
		rows, err := q.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return "", errOldRefused
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", errOldRefused
		}
		count := 0
		for rows.Next() {
			count++
			if count > rowLimit {
				_ = rows.Close()
				return "", errOldRefused
			}
			values := make([]string, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if rows.Scan(dest...) != nil {
				_ = rows.Close()
				return "", errOldRefused
			}
			raw, _ := json.Marshal(values)
			bytesRead += len(raw)
			if !check(raw, bytesRead) {
				_ = rows.Close()
				return "", errOldRefused
			}
			oldHashPart(h, raw)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", errOldRefused
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func oldPublicSnapshot(ctx context.Context, q gostore.Querier, session string) (string, error) {
	return oldDigest(ctx, q, []oldQuery{
		{`SELECT CAST(id AS TEXT),CASE WHEN length(CAST(coalesce(payload_json,'') AS BLOB))<=65536 THEN coalesce(payload_json,'') END FROM events WHERE session_id=? AND kind=? ORDER BY id LIMIT 1025`, []any{session, "session.turn_output"}},
		{`SELECT id,CASE WHEN length(CAST(coalesce(payload,'') AS BLOB))<=67108864 THEN coalesce(payload,'') END,content_type,CASE WHEN length(CAST(metadata AS BLOB))<=65536 THEN metadata END,channel,to_urn,coalesce(thread_id,''),kind,CAST(routing_staged AS TEXT) FROM messages WHERE ` + oldOutboxPredicate + ` ORDER BY id LIMIT 1025`, []any{"msg://session/local/" + session}},
		{`SELECT CAST(p.seq AS TEXT),p.name,p.message_id FROM channel_publications p JOIN messages m ON m.id=p.message_id WHERE m.` + oldOutboxPredicate + ` ORDER BY p.seq LIMIT 1025`, []any{"msg://session/local/" + session}},
	}, 1024, func(_ []byte, total int) bool { return total <= 128<<20 })
}

// oldMessagingSnapshot returns the old refusal code for anything pending, else the digest. As in
// the old code, a failed read is the messaging_unsupported refusal, not an error.
//
//nolint:nilerr // transcribed verbatim: the refusal code carries the failure
func oldMessagingSnapshot(ctx context.Context, q gostore.Querier, session string) (string, string, error) {
	var schema int
	if q.QueryRowContext(ctx, `SELECT version FROM messaging_delivery_schema WHERE component='delivery'`).Scan(&schema) != nil || (schema != 1 && schema != 2) {
		return "", "messaging_unsupported", nil
	}
	args := []any{"msg://session/local/" + session, session, session, session}
	var pending int
	if q.QueryRowContext(ctx, `SELECT count(*) FROM messaging_deliveries d WHERE (`+oldDeliveryPredicate+`) AND (d.status!='delivered' OR d.active_attempt_id!='' OR d.active_lease_token!='' OR d.lease_holder!='' OR NOT EXISTS(SELECT 1 FROM messaging_receipts r WHERE r.delivery_id=d.id AND r.stage='consumed' AND EXISTS(SELECT 1 FROM messaging_attempts a WHERE a.id=r.attempt_id AND a.delivery_id=d.id AND a.stage='consumed' AND a.consumed_at IS NOT NULL)))`, args...).Scan(&pending) != nil {
		return "", "messaging_unsupported", nil
	}
	if pending != 0 {
		return "", "ack_pending", nil
	}
	if q.QueryRowContext(ctx, `SELECT count(*) FROM messages m WHERE m.to_urn=? AND (m.consumed_at IS NULL OR m.delivery_id IS NULL OR NOT EXISTS(SELECT 1 FROM messaging_deliveries d WHERE d.id=m.delivery_id AND d.status='delivered'))`, args[0]).Scan(&pending) != nil {
		return "", "messaging_unsupported", nil
	}
	if pending != 0 {
		return "", "mail_pending", nil
	}
	if q.QueryRowContext(ctx, `SELECT count(*) FROM routing_replies WHERE (target_session_id=? OR delivered_to_session_id=?) AND state NOT IN ('delivered','undeliverable')`, session, session).Scan(&pending) != nil {
		return "", "messaging_unsupported", nil
	}
	if pending != 0 {
		return "", "reply_pending", nil
	}
	digest, err := oldDigest(ctx, q, []oldQuery{
		{`SELECT d.id,d.message_id,(SELECT digest FROM messaging_messages WHERE id=d.message_id),d.recipient_urn,d.binding_json,d.status,CAST(d.attempt_count AS TEXT),d.active_attempt_id,d.active_lease_token,d.lease_holder,coalesce(d.lease_expires_at,''),d.updated_at FROM messaging_deliveries d WHERE ` + oldDeliveryPredicate + ` ORDER BY d.id LIMIT 1025`, args},
		{`SELECT a.id,a.delivery_id,a.lease_token,a.holder,a.binding_json,CAST(a.binding_generation AS TEXT),a.acquired_at,a.expires_at,a.stage,coalesce(a.consumed_at,''),coalesce(a.failed_at,'') FROM messaging_attempts a JOIN messaging_deliveries d ON d.id=a.delivery_id WHERE ` + oldDeliveryPredicate + ` ORDER BY a.id LIMIT 1025`, args},
		{`SELECT CAST(r.id AS TEXT),r.delivery_id,r.attempt_id,r.stage,r.at FROM messaging_receipts r JOIN messaging_deliveries d ON d.id=r.delivery_id WHERE ` + oldDeliveryPredicate + ` ORDER BY r.id LIMIT 1025`, args},
		{`SELECT m.id,coalesce(m.delivery_id,''),coalesce(m.consumed_at,''),coalesce(m.pending_receipt_attempt_id,''),coalesce(m.pending_receipt_lease_token,'') FROM messages m WHERE m.to_urn=? ORDER BY m.id LIMIT 1025`, []any{args[0]}},
		{`SELECT reply_id,target_session_id,delivered_to_session_id,state,reason,CAST(attempts AS TEXT),updated_at,coalesce(settled_at,'') FROM routing_replies WHERE target_session_id=? OR delivered_to_session_id=? ORDER BY reply_id LIMIT 1025`, []any{session, session}},
	}, 1024, func(raw []byte, _ int) bool { return len(raw) <= 64<<10 })
	return digest, "", err
}

// ledgerPublicFixture writes the session's public ledger (and another session's noise): turn
// outputs (one with a \u0000 escape), a staged output, a published output with its publication,
// a message carrying an output_id, one whose output_id is JSON null, and invalid metadata.
func ledgerPublicFixture(t *testing.T, s *store.Stores) {
	t.Helper()
	for i, p := range []string{`{"output_id":"out-1"}`, `{"output_id":"out-2","text":"a\u0000b"}`} {
		repExec(t, s.B(), `INSERT INTO events (scope, session_id, kind, payload_json, at, recorded_at, event_id, pending)
 VALUES ('session', 'ses-1', 'session.turn_output', ?, '2026-11-14T00:00:00Z', '2026-11-14T00:00:00Z', ?, 0)`, p, "evt-"+string(rune('a'+i)))
	}
	repExec(t, s.B(), `INSERT INTO events (scope, session_id, kind, payload_json, at, recorded_at, event_id, pending)
 VALUES ('session', 'ses-2', 'session.turn_output', '{}', '2026-11-14T00:00:00Z', '2026-11-14T00:00:00Z', 'evt-other', 0)`)
	for _, m := range []struct{ id, to, meta, thread string }{
		{"m-staged", "msg://service/local/turn-output", `{"kind":"final"}`, "ses-1"},
		{"m-pub", "msg://service/local/channel/news", `{"output_id":"out-1","note":"a\u0000b"}`, "ses-1"},
		{"m-out", "msg://agent/x/y", `{"output_id":"out-2"}`, ""},
		{"m-null", "msg://agent/x/y", `{"output_id":null}`, ""},
		{"m-bad", "msg://agent/x/y", `not json`, ""},
	} {
		repExec(t, s.C(), `INSERT INTO messages (id, kind, channel, from_urn, to_urn, thread_id, payload, content_type, metadata, created_at, routing_staged)
 VALUES (?, 'notice', '', 'msg://session/local/ses-1', ?, ?, '{"text":"x"}', 'application/json', ?, '2026-11-14T00:00:00Z', ?)`,
			m.id, m.to, nullable(m.thread), m.meta, map[bool]int{true: 1, false: 0}[m.id == "m-staged"])
	}
	repExec(t, s.C(), `INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, content_type, metadata, created_at, routing_staged)
 VALUES ('m-other', 'notice', '', 'msg://session/local/ses-2', 'msg://service/local/turn-output', '{}', 'application/json', '{}', '2026-11-14T00:00:00Z', 1)`)
	repExec(t, s.C(), `INSERT INTO channel_publications (name, message_id) VALUES ('news', 'm-pub')`)
}

func nullable(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// TestShimCodexLedgerParity: on SQLite the public ledger digest equals the old store's over the
// same rows. On Postgres the old SQL cannot run (json_valid, json_extract); the new digest is the
// same queries' text in the same order and is checked stable and selective there.
func TestShimCodexLedgerParity(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, _ := newStore(s)
		repRunning(t, s, ss, "ses-1", 42)
		r := s.C().Reader()
		empty, err := sessionstore.CodexPublicLedgerDigest(ctx, r, "ses-1")
		if err != nil {
			t.Fatal(err)
		}
		ledgerPublicFixture(t, s)
		got, err := sessionstore.CodexPublicLedgerDigest(ctx, r, "ses-1")
		if err != nil || got == empty {
			t.Fatalf("digest %q (empty %q) %v", got, empty, err)
		}
		if outputs, messages, err := sessionstore.CodexPublicCounts(ctx, r, "ses-1"); err != nil || outputs != 2 || messages != 3 {
			t.Fatalf("counts %d %d %v", outputs, messages, err)
		}
		if s.C().Dialect().Name() != gostore.DriverSQLite {
			return
		}
		for _, session := range []string{"ses-1", "ses-2", "ses-none"} {
			want, err := oldPublicSnapshot(ctx, r, session)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := sessionstore.CodexPublicLedgerDigest(ctx, r, session); err != nil || got != want {
				t.Fatalf("%s: new %q, old %q (%v)", session, got, want, err)
			}
		}
	})
}

// TestShimCodexMessagingLedger: over real go-messaging rows (the library's own schema and store),
// the pending counts refuse in the old order and the settled ledger's digest equals the old
// store's. SQLite only: go-messaging has no Postgres store (CW-20261115-0013).
func TestShimCodexMessagingLedger(t *testing.T) {
	storetest.Dialects(t, func(t *testing.T, s *store.Stores) {
		ctx := context.Background()
		ss, clock := newStore(s)
		if s.C().Dialect().Name() == gostore.DriverPostgres {
			if _, _, _, err := sessionstore.CodexMessagingLedger(ctx, s.C().Reader(), "ses-1"); !errors.Is(err, store.ErrLibraryUnavailable) {
				t.Fatalf("postgres: %v", err)
			}
		}
		db := storetest.LibraryDB(t, s) // skips on Postgres with the notice
		if err := delivery.ApplySQLiteSchema(ctx, db); err != nil {
			t.Fatal(err)
		}
		ds := delivery.NewSQLiteStore(db)
		repRunning(t, s, ss, "ses-1", 42)
		r := s.C().Reader()
		to := messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: "ses-1"}
		from := messaging.Address{Kind: messaging.KindAgent, Authority: "cell", ID: "alice"}

		check := func(name string, wantPending sessionstore.CodexMessagingPending, wantCode string) {
			t.Helper()
			pending, digest, schemaOK, err := sessionstore.CodexMessagingLedger(ctx, r, "ses-1")
			oldDigestV, oldCode, oldErr := oldMessagingSnapshot(ctx, r, "ses-1")
			if err != nil || oldErr != nil || !schemaOK || pending != wantPending || oldCode != wantCode {
				t.Fatalf("%s: pending %+v schema %v err %v; old code %q err %v", name, pending, schemaOK, err, oldCode, oldErr)
			}
			if digest != oldDigestV {
				t.Fatalf("%s: new digest %q, old %q", name, digest, oldDigestV)
			}
		}
		check("empty", sessionstore.CodexMessagingPending{}, "")

		res, err := ds.Enqueue(ctx, delivery.EnqueueRequest{From: from, Recipients: []delivery.RecipientTarget{{Address: to}},
			Kind: messaging.MsgKindRequest, Payload: []byte(`{"body":"hi"}`), ContentType: "application/json"})
		if err != nil {
			t.Fatal(err)
		}
		deliveryID := res.Deliveries[0].ID
		repExec(t, s.C(), `INSERT INTO messages (id, kind, channel, from_urn, to_urn, payload, content_type, metadata, created_at, delivery_id)
 VALUES ('mail-1', 'request', '', ?, ?, '{"body":"hi"}', 'application/json', '{}', '2026-11-14T00:00:00Z', ?)`, from.URN(), to.URN(), string(deliveryID))
		check("delivery pending", sessionstore.CodexMessagingPending{Deliveries: 1, Mail: 1}, "ack_pending")

		claim, err := ds.Claim(ctx, delivery.ClaimRequest{DeliveryID: deliveryID, Holder: "ses-1", LeaseDuration: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		lease := delivery.LeaseRef{DeliveryID: deliveryID, AttemptID: claim.Attempt.ID, LeaseToken: claim.Attempt.LeaseToken}
		for _, stage := range []delivery.ReceiptStage{delivery.StageHostAccepted, delivery.StageTurnSubmitted, delivery.StageConsumed} {
			if _, _, err := ds.Ack(ctx, delivery.AckRequest{Lease: lease, Stage: stage}); err != nil {
				t.Fatalf("ack %s: %v", stage, err)
			}
		}
		check("mail pending", sessionstore.CodexMessagingPending{Mail: 1}, "mail_pending")

		repExec(t, s.C(), `UPDATE messages SET consumed_at = ? WHERE id = 'mail-1'`, clock.Now().Format(time.RFC3339Nano))
		repExec(t, s.C(), `INSERT INTO routing_replies (reply_id, parent_id, original_session_id, target_session_id, state, created_at, updated_at)
 VALUES ('rr-1', 'p', 'ses-1', 'ses-1', 'queued', 'x', 'x')`)
		check("reply pending", sessionstore.CodexMessagingPending{Replies: 1}, "reply_pending")

		repExec(t, s.C(), `UPDATE routing_replies SET state = 'delivered', settled_at = 'y' WHERE reply_id = 'rr-1'`)
		check("settled", sessionstore.CodexMessagingPending{}, "")
		_, settled, _, _ := sessionstore.CodexMessagingLedger(ctx, r, "ses-1")
		if settled == "" || strings.TrimSpace(settled) != settled {
			t.Fatalf("settled digest %q", settled)
		}
	})
}
