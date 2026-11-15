package msgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// MessageAlias is a readable alias for a message URN.
type MessageAlias struct {
	URN   string `json:"urn"`
	Alias string `json:"alias"`
}

var messageAliasPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,63}$`)

// SetMessageAlias sets (or replaces) the alias of a URN.
func (s *Store) SetMessageAlias(ctx context.Context, urn, alias string) error {
	address, err := messaging.ParseURN(urn)
	if err != nil {
		return fmt.Errorf("invalid alias URN: %w", err)
	}
	alias = strings.TrimSpace(alias)
	if !messageAliasPattern.MatchString(alias) {
		return errors.New("alias must start with a letter and contain at most 64 letters, digits, dots, underscores or hyphens")
	}
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO message_aliases (urn, alias) VALUES (?, ?) ON CONFLICT (urn) DO UPDATE SET alias = excluded.alias`),
			address.URN(), alias)
		return err
	})
}

// aliasFold is the alias column folded as SQLite's NOCASE folds it (ASCII only): lower() on SQLite;
// on Postgres lower() under COLLATE "C", since the default collation would also fold non-ASCII
// letters (U+212A KELVIN SIGN to k) and match aliases SQLite would not. param is the same for a
// bound value.
func aliasFold(q gostore.Querier) (column, param string) {
	if q.Dialect().Name() == gostore.DriverPostgres {
		return `lower(alias COLLATE "C")`, `lower(CAST(? AS text) COLLATE "C")`
	}
	return `lower(alias)`, `lower(?)`
}

// ListMessageAliases returns every alias, ordered case-insensitively (COLLATE NOCASE in the old
// store; aliasFold here), then by byte order.
func (s *Store) ListMessageAliases(ctx context.Context) ([]MessageAlias, error) {
	r := s.c.Reader()
	column, _ := aliasFold(r)
	order := column + ", alias"
	if r.Dialect().Name() == gostore.DriverPostgres {
		order = column + `, alias COLLATE "C"`
	}
	rows, err := r.QueryContext(ctx, `SELECT urn, alias FROM message_aliases ORDER BY `+order)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []MessageAlias{}
	for rows.Next() {
		var a MessageAlias
		if err := rows.Scan(&a.URN, &a.Alias); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ResolveMessageAddress parses a msg:// URN, or resolves an alias (with or without a leading @,
// case-insensitive) to its URN.
func (s *Store) ResolveMessageAddress(ctx context.Context, value string) (messaging.Address, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "msg://") {
		return messaging.ParseURN(value)
	}
	r := s.c.Reader()
	var urn string
	column, param := aliasFold(r)
	err := r.QueryRowContext(ctx, rebind(r, `SELECT urn FROM message_aliases WHERE `+column+` = `+param), strings.TrimPrefix(value, "@")).Scan(&urn)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Address{}, fmt.Errorf("unknown message alias %q", value)
	}
	if err != nil {
		return messaging.Address{}, err
	}
	return messaging.ParseURN(urn)
}

// PayloadWithContentType returns a message's payload when its content type is contentType (the
// typed-envelope lookup behind the old messageEnvelopeFields; parsing it is the messaging slice's).
func (s *Store) PayloadWithContentType(ctx context.Context, messageID, contentType string) (string, bool, error) {
	if messageID == "" {
		return "", false, nil
	}
	r := s.c.Reader()
	var payload string
	err := r.QueryRowContext(ctx, rebind(r, `SELECT COALESCE(payload,'') FROM messages WHERE id = ? AND content_type = ?`), messageID, contentType).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return payload, err == nil, err
}

// ReplyCandidate is a typed message that may await a reply from the seat.
type ReplyCandidate struct {
	MessageID string
	From      string
	Payload   string
	CreatedAt time.Time
}

// pgPayload is a message payload with each \u0000 escape replaced, for a jsonb cast on Postgres
// (see PendingReplyCandidates).
const pgPayload = `replace(m.payload, '\u0000', '\ufffd')`

// PendingReplyCandidates returns, newest first, typed messages (contentType) to any of the seat's
// URNs, not canceled, archived or staged, with no reply from the seat yet, whose payload is a JSON
// object that either names handle in awaiting, or awaits nobody and has a kind in replyKinds. The
// messaging slice parses each candidate and applies the envelope's own rule (NeedsReply in the old
// tree). On Postgres the JSON is read under CASE WHEN ... IS JSON OBJECT, so an invalid payload is
// skipped as json_valid skipped it on SQLite (a WHERE conjunction has no evaluation order there).
// Postgres refuses a \u0000 escape in json and jsonb values, which IS JSON and SQLite accept, so
// one such payload would fail the whole list: pgPayload replaces the escape with U+FFFD before the
// cast. Only kind and awaiting are read, and neither ever matches a value holding NUL, so the result
// is SQLite's. pg_input_is_valid catches anything else jsonb refuses (a lone surrogate escape).
func (s *Store) PendingReplyCandidates(ctx context.Context, urns []string, handle, contentType string, replyKinds []string, limit int) ([]ReplyCandidate, error) {
	if len(urns) == 0 || len(replyKinds) == 0 {
		return []ReplyCandidate{}, nil
	}
	if limit <= 0 || limit > 50 {
		limit = 50
	}
	r := s.c.Reader()
	in := placeholders(len(urns))
	var awaiting, kind, valid string
	if r.Dialect().Name() == gostore.DriverPostgres {
		valid = `(m.payload IS JSON OBJECT AND pg_input_is_valid(` + pgPayload + `, 'jsonb'))`
		awaiting = `(CASE WHEN ` + valid + ` THEN (` + pgPayload + `)::jsonb->>'awaiting' END)`
		kind = `(CASE WHEN ` + valid + ` THEN (` + pgPayload + `)::jsonb->>'kind' END)`
	} else {
		awaiting = `json_extract(m.payload,'$.awaiting')`
		kind = `json_extract(m.payload,'$.kind')`
		valid = `json_valid(m.payload)`
	}
	q := `SELECT m.id, m.from_urn, COALESCE(m.payload,''), m.created_at FROM messages m
 WHERE m.to_urn IN (` + in + `) AND m.content_type = ? AND m.canceled_at IS NULL AND m.archived_at IS NULL AND m.routing_staged = 0
 AND ` + valid + `
 AND (` + awaiting + ` = ? OR (COALESCE(` + awaiting + `,'') = '' AND ` + kind + ` IN (` + placeholders(len(replyKinds)) + `)))
 AND NOT EXISTS (SELECT 1 FROM messages rp WHERE rp.in_reply_to = m.id AND rp.from_urn IN (` + in + `))
 ORDER BY m.created_at DESC, m.id DESC LIMIT ?`
	args := make([]any, 0, 2*len(urns)+len(replyKinds)+3)
	for _, u := range urns {
		args = append(args, u)
	}
	args = append(args, contentType, handle)
	for _, k := range replyKinds {
		args = append(args, k)
	}
	for _, u := range urns {
		args = append(args, u)
	}
	args = append(args, limit)
	rows, err := r.QueryContext(ctx, rebind(r, q), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := []ReplyCandidate{}
	for rows.Next() {
		var c ReplyCandidate
		var created string
		if err := rows.Scan(&c.MessageID, &c.From, &c.Payload, &created); err != nil {
			return nil, err
		}
		c.CreatedAt = parseTime(created)
		out = append(out, c)
	}
	return out, rows.Err()
}

// BrokerEnvelope is a legacy v002 broker_envelopes row (broker.Envelope in the old tree). Times are
// stored text, as before.
type BrokerEnvelope struct {
	ID, Sender, Recipient, WorkflowID, CorrelationID, MessageType string
	Priority                                                      int
	Payload, CreatedAt, DeliveredAt, ConsumedAt, AuditJSON        string
}

const brokerColumns = `id, sender, recipient, workflow_id, correlation_id, message_type, priority, payload, created_at, delivered_at, consumed_at, audit_json`

// CreateBrokerEnvelope stores an envelope; id and created_at are required.
func (s *Store) CreateBrokerEnvelope(ctx context.Context, e BrokerEnvelope) error {
	if e.ID == "" {
		return errors.New("envelope id required")
	}
	if e.CreatedAt == "" {
		return errors.New("envelope created_at required")
	}
	err := s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO broker_envelopes (`+brokerColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			e.ID, nullText(e.Sender), nullText(e.Recipient), nullText(e.WorkflowID), nullText(e.CorrelationID), nullText(e.MessageType),
			e.Priority, nullText(e.Payload), e.CreatedAt, nullText(e.DeliveredAt), nullText(e.ConsumedAt), nullText(e.AuditJSON))
		return err
	})
	if err != nil {
		return fmt.Errorf("insert envelope %q: %w", e.ID, err)
	}
	return nil
}

func scanBroker(scan scanFunc) (BrokerEnvelope, error) {
	var e BrokerEnvelope
	var sender, recipient, workflow, corr, msgType, payload, delivered, consumed, audit sql.NullString
	var priority sql.NullInt64
	if err := scan(&e.ID, &sender, &recipient, &workflow, &corr, &msgType, &priority, &payload, &e.CreatedAt, &delivered, &consumed, &audit); err != nil {
		return BrokerEnvelope{}, err
	}
	e.Sender, e.Recipient, e.WorkflowID, e.CorrelationID, e.MessageType = sender.String, recipient.String, workflow.String, corr.String, msgType.String
	e.Priority = int(priority.Int64)
	e.Payload, e.DeliveredAt, e.ConsumedAt, e.AuditJSON = payload.String, delivered.String, consumed.String, audit.String
	return e, nil
}

// GetBrokerEnvelope returns one envelope (sql.ErrNoRows when absent, as before).
func (s *Store) GetBrokerEnvelope(ctx context.Context, id string) (BrokerEnvelope, error) {
	r := s.c.Reader()
	return scanBroker(r.QueryRowContext(ctx, rebind(r, `SELECT `+brokerColumns+` FROM broker_envelopes WHERE id = ?`), id).Scan)
}

func (s *Store) listBroker(ctx context.Context, where string, args ...any) ([]BrokerEnvelope, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+brokerColumns+` FROM broker_envelopes WHERE `+where+` ORDER BY created_at ASC, id ASC`), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []BrokerEnvelope
	for rows.Next() {
		e, err := scanBroker(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("scan envelope: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListBrokerEnvelopesByRecipient returns a recipient's undelivered envelopes, oldest first.
func (s *Store) ListBrokerEnvelopesByRecipient(ctx context.Context, recipient string) ([]BrokerEnvelope, error) {
	return s.listBroker(ctx, `recipient = ? AND delivered_at IS NULL`, recipient)
}

// ListBrokerEnvelopesByWorkflow returns a workflow's envelopes (narrowed by correlation id when
// set), oldest first.
func (s *Store) ListBrokerEnvelopesByWorkflow(ctx context.Context, workflowID, correlationID string) ([]BrokerEnvelope, error) {
	if correlationID == "" {
		return s.listBroker(ctx, `workflow_id = ?`, workflowID)
	}
	return s.listBroker(ctx, `workflow_id = ? AND correlation_id = ?`, workflowID, correlationID)
}

// RoutingStageRetention is how long an unpromoted routing-staged message is kept before its body
// may be purged.
const RoutingStageRetention = 30 * 24 * time.Hour

// ErrPendingObligation is returned when the message still has a delivery obligation.
var ErrPendingObligation = errors.New("store: retention: message has a pending delivery obligation")

// DeliveryStatus looks up a go-messaging delivery's status (the delivery library's store, which the
// messaging slice opens). ok=false means unknown.
type DeliveryStatus func(ctx context.Context, deliveryID string) (status string, ok bool, err error)

// RetentionCandidate is a message old enough to consider for a body purge.
type RetentionCandidate struct {
	MessageID   string
	CreatedAt   time.Time
	HasDelivery bool
	Status      string
	Eligible    bool
}

// purgeEligibleStatus: a delivery that is delivered or canceled has no obligation left.
func purgeEligibleStatus(st string) bool { return st == "delivered" || st == "canceled" }

type retentionRow struct {
	id, created         string
	consumed, canceled  sql.NullString
	deliveryID          sql.NullString
	staged, publication bool
	payload, metadata   sql.NullString
}

func (s *Store) eligible(ctx context.Context, r retentionRow, status DeliveryStatus, strict bool) (st string, ok bool, err error) {
	switch {
	case r.deliveryID.String != "":
		if status == nil {
			return "", false, fmt.Errorf("store: retention: message %s has a delivery and no status lookup", r.id)
		}
		st, found, err := status(ctx, r.deliveryID.String)
		if err != nil {
			if strict {
				return "", false, fmt.Errorf("store: retention: purge %s: get delivery: %w", r.id, err)
			}
			return "", false, nil
		}
		if !found {
			if strict { // the old purge failed on its delivery lookup, not as a pending obligation
				return "", false, fmt.Errorf("store: retention: purge %s: get delivery: %w", r.id, messaging.ErrNotFound)
			}
			return "", false, nil
		}
		return st, purgeEligibleStatus(st), nil
	case r.staged:
		t := parseTime(r.created)
		return "", !t.IsZero() && !t.After(s.now().Add(-RoutingStageRetention)), nil
	default:
		return "", r.publication || r.consumed.String != "" || r.canceled.String != "", nil
	}
}

// ListRetentionCandidates returns every message created before olderThan with its purge
// eligibility: a delivery-backed message by its delivery status, a staged one by age, any other by
// being a publication, consumed or canceled.
func (s *Store) ListRetentionCandidates(ctx context.Context, olderThan time.Time, status DeliveryStatus) ([]RetentionCandidate, error) {
	q := s.c.Reader()
	rows, err := q.QueryContext(ctx, rebind(q, `SELECT id, created_at, consumed_at, canceled_at, delivery_id, routing_staged,
 EXISTS (SELECT 1 FROM channel_publications p WHERE p.message_id = messages.id)
 FROM messages WHERE created_at < ? ORDER BY created_at ASC, id ASC`), stamp(olderThan))
	if err != nil {
		return nil, fmt.Errorf("store: retention: list candidates: %w", err)
	}
	var raw []retentionRow
	for rows.Next() {
		var r retentionRow
		var staged int64
		if err := rows.Scan(&r.id, &r.created, &r.consumed, &r.canceled, &r.deliveryID, &staged, &r.publication); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("store: retention: scan candidate: %w", err)
		}
		r.staged = staged != 0
		raw = append(raw, r)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]RetentionCandidate, 0, len(raw))
	for _, r := range raw {
		c := RetentionCandidate{MessageID: r.id, CreatedAt: parseTime(r.created), HasDelivery: r.deliveryID.String != ""}
		if c.Status, c.Eligible, err = s.eligible(ctx, r, status, false); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// PurgeMessageBody clears a message's payload and metadata when it has no obligation left, and
// records who authorized it in message_purge_audit, in one class C transaction. purged=false with
// no error means there was nothing left to clear.
func (s *Store) PurgeMessageBody(ctx context.Context, messageID, authorizedBy string, status DeliveryStatus) (purged bool, err error) {
	if _, err := messaging.ParseURN(authorizedBy); err != nil {
		return false, fmt.Errorf("store: retention: authorized_by must be a valid URN: %w", err)
	}
	q := s.c.Reader()
	r := retentionRow{id: messageID}
	var staged int64
	err = q.QueryRowContext(ctx, rebind(q, `SELECT payload, metadata, consumed_at, canceled_at, delivery_id, routing_staged, created_at,
 EXISTS (SELECT 1 FROM channel_publications p WHERE p.message_id = messages.id) FROM messages WHERE id = ?`), messageID).
		Scan(&r.payload, &r.metadata, &r.consumed, &r.canceled, &r.deliveryID, &staged, &r.created, &r.publication)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("store: retention: purge %s: %w", messageID, messaging.ErrNotFound)
	}
	if err != nil {
		return false, fmt.Errorf("store: retention: purge %s: %w", messageID, err)
	}
	r.staged = staged != 0
	_, ok, err := s.eligible(ctx, r, status, true)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, ErrPendingObligation
	}
	if !r.payload.Valid && !r.metadata.Valid {
		return false, nil
	}
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		// The old query bound a Go bool against 0; Postgres will not compare a boolean with an
		// integer, so the staged flag is bound as the integer it is stored as.
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET payload = NULL, metadata = NULL
 WHERE id = ? AND (payload IS NOT NULL OR metadata IS NOT NULL) AND (? = 0 OR routing_staged = 1)`), messageID, staged)
		if err != nil {
			return fmt.Errorf("store: retention: purge %s: %w", messageID, err)
		}
		n, err := res.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO message_purge_audit (at, table_name, message_id, authorized_by) VALUES (?, 'messages', ?, ?)`),
			stamp(s.now()), messageID, authorizedBy); err != nil {
			return fmt.Errorf("store: retention: audit purge %s: %w", messageID, err)
		}
		purged = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return purged, nil
}
