package sessionstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"strconv"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
)

// The Codex replacement accounting (old codex_replacement_accounting.go) proves that nothing of an
// old Codex session's public output, mail or delivery is left unaccounted before its replacement.
// Reading the provider journal and retry files, projecting the history and deciding each refusal
// are the shim slice's (S9). The store holds the census queries: counts, and digests binding the
// ledgers, read on the q the caller passes (a read snapshot when issuing the proof, the replacement
// joint's transaction when validating it, so the proof binds the state the replacement commits).

// ErrCodexLedgerBudget is returned when a ledger exceeds its row or byte budget (history_budget).
var ErrCodexLedgerBudget = errors.New("codex ledger over budget")

// CodexLedgerTurnOutputKind is the event kind of a published turn output (events.KindSessionTurnOutput).
const CodexLedgerTurnOutputKind = "session.turn_output"

// CodexLedgerRows is the most rows one ledger query may return (the old accountingTurns).
const CodexLedgerRows = 1024

// codexOutboxPredicate selects the session's outbox messages: turn outputs (staged or published)
// and messages carrying an output_id (old accountingOutboxPredicate).
func codexOutboxPredicate(q gostore.Querier, alias string) string {
	col := alias + "metadata"
	if q.Dialect().Name() == gostore.DriverPostgres {
		md := `replace(` + col + `, '\u0000', '�')`
		return alias + `from_urn = ? AND (` + alias + `to_urn = 'msg://service/local/turn-output' OR (CASE WHEN pg_input_is_valid(` + md + `, 'jsonb') THEN (` + md + `)::jsonb->>'output_id' END) IS NOT NULL)`
	}
	return alias + `from_urn = ? AND (` + alias + `to_urn = 'msg://service/local/turn-output' OR CASE WHEN json_valid(` + col + `) THEN json_extract(` + col + `, '$.output_id') END IS NOT NULL)`
}

// codexBounded is column when its byte length is at most n, NULL otherwise.
func codexBounded(q gostore.Querier, column string, n int) string {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return `CASE WHEN octet_length(` + column + `) <= ` + strconv.Itoa(n) + ` THEN ` + column + ` END`
	}
	return `CASE WHEN length(CAST(` + column + ` AS BLOB)) <= ` + strconv.Itoa(n) + ` THEN ` + column + ` END`
}

// codexHashPart frames one row in the ledger digest (length, then bytes).
func codexHashPart(h hash.Hash, part []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(part)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(part)
}

type codexLedgerQuery struct {
	sql  string
	args []any
}

// codexLedgerDigest hashes every row of the queries, each row as the JSON array of its columns
// read as text (a NULL fails the read), within maxRows per query, maxRowBytes per row and maxTotal bytes.
func codexLedgerDigest(ctx context.Context, q gostore.Querier, queries []codexLedgerQuery, maxRowBytes, maxTotal int) (string, error) {
	h := sha256.New()
	total := 0
	for _, query := range queries {
		rows, err := q.QueryContext(ctx, rebind(q, query.sql), query.args...)
		if err != nil {
			return "", err
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", err
		}
		count := 0
		for rows.Next() {
			if count++; count > CodexLedgerRows {
				_ = rows.Close()
				return "", ErrCodexLedgerBudget
			}
			values := make([]string, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			// A NULL column (an oversized bounded value) fails the scan, as before: it refuses.
			if err := rows.Scan(dest...); err != nil {
				_ = rows.Close()
				return "", err
			}
			raw, _ := json.Marshal(values)
			total += len(raw)
			if (maxRowBytes > 0 && len(raw) > maxRowBytes) || (maxTotal > 0 && total > maxTotal) {
				_ = rows.Close()
				return "", ErrCodexLedgerBudget
			}
			codexHashPart(h, raw)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// CodexPublicLedgerDigest binds every public acceptance and outbox row of the session: its turn
// output events (class B), its outbox messages and their channel publications (old
// accountingPublicSnapshot, byte-identical on SQLite: same queries, column text, row framing and
// budgets). An oversized payload or metadata is NULL (the bound keeps it out of
// the process) and fails the read, as before; more than CodexLedgerRows rows in one ledger, or 128 MiB in all, is
// ErrCodexLedgerBudget.
func CodexPublicLedgerDigest(ctx context.Context, q gostore.Querier, sessionID string) (string, error) {
	from := "msg://session/local/" + sessionID
	return codexLedgerDigest(ctx, q, []codexLedgerQuery{
		{`SELECT CAST(id AS TEXT), ` + codexBounded(q, "COALESCE(payload_json,'')", 65536) + ` FROM events WHERE session_id = ? AND kind = ? ORDER BY id LIMIT 1025`,
			[]any{sessionID, CodexLedgerTurnOutputKind}},
		{`SELECT id, ` + codexBounded(q, "COALESCE(payload,'')", 67108864) + `, content_type, ` + codexBounded(q, "metadata", 65536) +
			`, channel, to_urn, COALESCE(thread_id,''), kind, CAST(routing_staged AS TEXT) FROM messages WHERE ` + codexOutboxPredicate(q, "") + ` ORDER BY id LIMIT 1025`,
			[]any{from}},
		{`SELECT CAST(p.seq AS TEXT), p.name, p.message_id FROM channel_publications p JOIN messages m ON m.id = p.message_id WHERE ` +
			codexOutboxPredicate(q, "m.") + ` ORDER BY p.seq LIMIT 1025`, []any{from}},
	}, 0, 128<<20)
}

// CodexPublicCounts returns how many turn output events and outbox messages the session has.
func CodexPublicCounts(ctx context.Context, q gostore.Querier, sessionID string) (outputs, messages int, err error) {
	if err = q.QueryRowContext(ctx, rebind(q, `SELECT COUNT(*) FROM events WHERE session_id = ? AND kind = ?`), sessionID, CodexLedgerTurnOutputKind).Scan(&outputs); err != nil {
		return 0, 0, err
	}
	err = q.QueryRowContext(ctx, rebind(q, `SELECT COUNT(*) FROM messages WHERE `+codexOutboxPredicate(q, "")), "msg://session/local/"+sessionID).Scan(&messages)
	return outputs, messages, err
}

// CodexTurnOutputPayload returns the payload of the session's turn output event with output_id id
// (found false when none). On Postgres the payload is read under the pg_input_is_valid guard, after
// each \u0000 escape is replaced with U+FFFD.
func CodexTurnOutputPayload(ctx context.Context, q gostore.Querier, sessionID, outputID string) (payload string, found bool, err error) {
	out := `CASE WHEN json_valid(payload_json) THEN json_extract(payload_json, '$.output_id') END`
	if q.Dialect().Name() == gostore.DriverPostgres {
		const p = `replace(payload_json, '\u0000', '�')`
		out = `CASE WHEN pg_input_is_valid(` + p + `, 'jsonb') THEN (` + p + `)::jsonb->>'output_id' END`
	}
	err = q.QueryRowContext(ctx, rebind(q, `SELECT payload_json FROM events WHERE session_id = ? AND kind = ? AND `+out+` = ? ORDER BY id LIMIT 1`),
		sessionID, CodexLedgerTurnOutputKind, outputID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return payload, err == nil, err
}

// CodexOutputMessage is a turn output's outbox message as the accounting checks it.
type CodexOutputMessage struct {
	Staged                bool // routing_staged
	Channel, To, ThreadID string
	Published             bool // it has a channel publication (Channel is its name)
}

// CodexOutputMessageTx reads the message (found false when it is missing or its payload purged)
// and its publication.
func CodexOutputMessageTx(ctx context.Context, q gostore.Querier, messageID string) (CodexOutputMessage, bool, error) {
	var m CodexOutputMessage
	var staged int64
	err := q.QueryRowContext(ctx, rebind(q, `SELECT routing_staged FROM messages WHERE id = ? AND payload IS NOT NULL`), messageID).Scan(&staged)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	m.Staged = staged != 0
	var thread sql.NullString
	err = q.QueryRowContext(ctx, rebind(q, `SELECT p.name, m.to_urn, m.thread_id FROM channel_publications p JOIN messages m ON m.id = p.message_id WHERE p.message_id = ?`), messageID).
		Scan(&m.Channel, &m.To, &thread)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return m, false, err
	default:
		m.Published, m.ThreadID = true, thread.String
	}
	return m, true, nil
}

// CodexMessagingPending counts what still holds an old Codex session's mail: deliveries not
// consumed through a real consumed receipt (or still leased), addressed mail not consumed through
// a delivered delivery, and routing replies not settled.
type CodexMessagingPending struct {
	Deliveries, Mail, Replies int
}

// codexDeliveryPredicate selects the deliveries associated with the old session: its recipient,
// its binding, or any attempt it held (old accountingDeliveryPredicate). go-messaging's tables are
// SQLite-only, so this is SQLite SQL.
const codexDeliveryPredicate = `d.recipient_urn = ? OR CASE WHEN json_valid(d.binding_json) THEN json_extract(d.binding_json, '$.session_id') END = ?
 OR EXISTS (SELECT 1 FROM messaging_attempts prior WHERE prior.delivery_id = d.id
 AND (prior.holder = ? OR CASE WHEN json_valid(prior.binding_json) THEN json_extract(prior.binding_json, '$.session_id') END = ?))`

// CodexMessagingLedger returns the pending counts and, when nothing is pending, a digest binding
// every delivery, attempt and receipt associated with the session, its addressed mail and its
// routing replies (old accountingMessagingSnapshot; the digest is byte-identical to its, being the
// same queries, column text, row framing and budgets). It reads go-messaging's tables on q, so it is SQLite-only
// (store.ErrLibraryUnavailable on Postgres, until libs gap CW-20261115-0013); a delivery schema
// version other than 1 or 2 is refused (schemaOK false), so a future schema cannot weaken custody.
func CodexMessagingLedger(ctx context.Context, q gostore.Querier, sessionID string) (pending CodexMessagingPending, digest string, schemaOK bool, err error) {
	if q.Dialect().Name() != gostore.DriverSQLite {
		return pending, "", false, store.ErrLibraryUnavailable
	}
	var schema int
	if err := q.QueryRowContext(ctx, `SELECT version FROM messaging_delivery_schema WHERE component = 'delivery'`).Scan(&schema); err != nil {
		return pending, "", false, err
	}
	if schema != 1 && schema != 2 {
		return pending, "", false, nil
	}
	to := "msg://session/local/" + sessionID
	args := []any{to, sessionID, sessionID, sessionID}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM messaging_deliveries d WHERE (`+codexDeliveryPredicate+`)
 AND (d.status != 'delivered' OR d.active_attempt_id != '' OR d.active_lease_token != '' OR d.lease_holder != ''
 OR NOT EXISTS (SELECT 1 FROM messaging_receipts r WHERE r.delivery_id = d.id AND r.stage = 'consumed'
 AND EXISTS (SELECT 1 FROM messaging_attempts a WHERE a.id = r.attempt_id AND a.delivery_id = d.id AND a.stage = 'consumed' AND a.consumed_at IS NOT NULL)))`, args...).
		Scan(&pending.Deliveries); err != nil {
		return pending, "", true, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages m WHERE m.to_urn = ? AND (m.consumed_at IS NULL OR m.delivery_id IS NULL
 OR NOT EXISTS (SELECT 1 FROM messaging_deliveries d WHERE d.id = m.delivery_id AND d.status = 'delivered'))`, to).Scan(&pending.Mail); err != nil {
		return pending, "", true, err
	}
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM routing_replies WHERE (target_session_id = ? OR delivered_to_session_id = ?)
 AND state NOT IN ('delivered','undeliverable')`, sessionID, sessionID).Scan(&pending.Replies); err != nil {
		return pending, "", true, err
	}
	// As before, anything pending refuses before the ledger is hashed (the caller checks Deliveries,
	// then Mail, then Replies); the digest is only taken over a settled ledger.
	if pending != (CodexMessagingPending{}) {
		return pending, "", true, nil
	}
	digest, err = codexLedgerDigest(ctx, q, []codexLedgerQuery{
		{`SELECT d.id, d.message_id, (SELECT digest FROM messaging_messages WHERE id = d.message_id), d.recipient_urn, d.binding_json, d.status,
 CAST(d.attempt_count AS TEXT), d.active_attempt_id, d.active_lease_token, d.lease_holder, COALESCE(d.lease_expires_at,''), d.updated_at
 FROM messaging_deliveries d WHERE ` + codexDeliveryPredicate + ` ORDER BY d.id LIMIT 1025`, args},
		{`SELECT a.id, a.delivery_id, a.lease_token, a.holder, a.binding_json, CAST(a.binding_generation AS TEXT), a.acquired_at, a.expires_at,
 a.stage, COALESCE(a.consumed_at,''), COALESCE(a.failed_at,'') FROM messaging_attempts a JOIN messaging_deliveries d ON d.id = a.delivery_id
 WHERE ` + codexDeliveryPredicate + ` ORDER BY a.id LIMIT 1025`, args},
		{`SELECT CAST(r.id AS TEXT), r.delivery_id, r.attempt_id, r.stage, r.at FROM messaging_receipts r JOIN messaging_deliveries d ON d.id = r.delivery_id
 WHERE ` + codexDeliveryPredicate + ` ORDER BY r.id LIMIT 1025`, args},
		{`SELECT m.id, COALESCE(m.delivery_id,''), COALESCE(m.consumed_at,''), COALESCE(m.pending_receipt_attempt_id,''), COALESCE(m.pending_receipt_lease_token,'')
 FROM messages m WHERE m.to_urn = ? ORDER BY m.id LIMIT 1025`, []any{to}},
		{`SELECT reply_id, target_session_id, delivered_to_session_id, state, reason, CAST(attempts AS TEXT), updated_at, COALESCE(settled_at,'')
 FROM routing_replies WHERE target_session_id = ? OR delivered_to_session_id = ? ORDER BY reply_id LIMIT 1025`, []any{sessionID, sessionID}},
	}, 64<<10, 0)
	return pending, digest, true, err
}
