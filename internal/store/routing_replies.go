package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/go-messaging"
)

// RoutingReplyState is where a reply to a routed message stands. The dispatcher
// moves it queued -> delivering -> delivered, or to undeliverable with a reason;
// it never deletes the row (ADR 0049 s1.6).
type RoutingReplyState string

const (
	RoutingReplyQueued        RoutingReplyState = "queued"
	RoutingReplyDelivering    RoutingReplyState = "delivering"
	RoutingReplyDelivered     RoutingReplyState = "delivered"
	RoutingReplyUndeliverable RoutingReplyState = "undeliverable"
)

// Terminal reports whether the dispatcher is finished with the reply.
func (s RoutingReplyState) Terminal() bool {
	return s == RoutingReplyDelivered || s == RoutingReplyUndeliverable
}

var (
	ErrRoutingReplyNotFound            = errors.New("routing reply: not found")
	ErrRoutingReplyIdempotencyConflict = errors.New("routing reply: idempotency key reused with a different reply")
	ErrRoutingReplyBodyPurged          = errors.New("routing reply: body was purged")
	// ErrRoutingReplyState means a transition was refused because the reply is
	// no longer in the state the caller expected (a concurrent drain won).
	ErrRoutingReplyState = errors.New("routing reply: not in the expected state")
)

// RoutingReplyAddress is where a reply envelope is addressed. A service
// address has no inbox, so the reply is visible to thread/trace readers but is
// never a second copy of the turn the dispatcher injects.
var RoutingReplyAddress = messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "routing-reply"}

// RoutingReply is the durable queue row and the delivery state consumers read.
type RoutingReply struct {
	ReplyID              string
	ParentID             string
	OriginalSessionID    string
	TargetSessionID      string
	DeliveredToSessionID string
	LogicalAgentID       string
	Actor                string
	Interrupt            bool
	State                RoutingReplyState
	Reason               string
	Attempts             int
	NextAttemptAt        *time.Time
	CreatedAt            time.Time
	UpdatedAt            time.Time
	SettledAt            *time.Time
}

// NewRoutingReply is one accepted reply.
type NewRoutingReply struct {
	From            messaging.Address
	ParentID        string
	ThreadID        string
	Body            string
	TargetSessionID string
	LogicalAgentID  string
	Actor           string
	Interrupt       bool
	IdempotencyKey  string
	Metadata        map[string]string
}

func (n NewRoutingReply) hash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%v\x00%s", n.ParentID, n.Body, n.Interrupt, n.TargetSessionID)))
	return hex.EncodeToString(sum[:])
}

const routingReplyColumns = `reply_id, parent_id, original_session_id, target_session_id, delivered_to_session_id,
 logical_agent_id, actor, interrupt, state, reason, attempts, next_attempt_at, created_at, updated_at, settled_at`

func scanRoutingReply(scan scanFunc) (RoutingReply, error) {
	var (
		r                      RoutingReply
		state                  string
		created, updated       string
		nextAttempt, settledAt sql.NullString
	)
	if err := scan(&r.ReplyID, &r.ParentID, &r.OriginalSessionID, &r.TargetSessionID, &r.DeliveredToSessionID,
		&r.LogicalAgentID, &r.Actor, &r.Interrupt, &state, &r.Reason, &r.Attempts, &nextAttempt, &created, &updated, &settledAt); err != nil {
		return RoutingReply{}, err
	}
	r.State = RoutingReplyState(state)
	r.CreatedAt, r.UpdatedAt = parseMsgTime(created), parseMsgTime(updated)
	r.NextAttemptAt, r.SettledAt = parseMsgNullTime(nextAttempt), parseMsgNullTime(settledAt)
	return r, nil
}

func routingNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// CreateRoutingReply stores the reply body once and queues it, in one
// transaction. created=false means the idempotency key matched an earlier,
// identical reply, which is returned unchanged; a different reply under the
// same key is ErrRoutingReplyIdempotencyConflict.
func (s *Store) CreateRoutingReply(ctx context.Context, in NewRoutingReply) (reply RoutingReply, created bool, err error) {
	if in.ParentID == "" || in.TargetSessionID == "" {
		return RoutingReply{}, false, fmt.Errorf("routing reply: parent and target session are required")
	}
	if _, err := messaging.ParseURN(in.From.URN()); err != nil {
		return RoutingReply{}, false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RoutingReply{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	hash := in.hash()
	if in.IdempotencyKey != "" {
		var priorHash string
		row := tx.QueryRowContext(ctx, `SELECT `+routingReplyColumns+`, request_hash FROM routing_replies
 WHERE parent_id=? AND actor=? AND idempotency_key=?`, in.ParentID, in.Actor, in.IdempotencyKey)
		prior, scanErr := scanRoutingReplyWithHash(row.Scan, &priorHash)
		switch {
		case scanErr == nil && priorHash == hash:
			return prior, false, nil
		case scanErr == nil:
			return RoutingReply{}, false, ErrRoutingReplyIdempotencyConflict
		case !errors.Is(scanErr, sql.ErrNoRows):
			return RoutingReply{}, false, scanErr
		}
	}

	id, err := uuid.NewV7()
	if err != nil {
		return RoutingReply{}, false, err
	}
	now := routingNow()
	body, err := json.Marshal(struct {
		Body string `json:"body"`
	}{in.Body})
	if err != nil {
		return RoutingReply{}, false, err
	}
	meta := map[string]string{"parent_id": in.ParentID, "target_session_id": in.TargetSessionID, "routing_reply": "1"}
	for k, v := range in.Metadata {
		if _, reserved := meta[k]; !reserved {
			meta[k] = v
		}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return RoutingReply{}, false, err
	}
	thread := in.ThreadID
	if thread == "" {
		thread = in.ParentID
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO messages
 (id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type, metadata, created_at)
 VALUES (?, ?, '', ?, ?, ?, ?, ?, 'application/json', ?, ?)`,
		id.String(), string(messaging.MsgKindResponse), in.From.URN(), RoutingReplyAddress.URN(),
		thread, in.ParentID, string(body), string(metaJSON), now); err != nil {
		return RoutingReply{}, false, fmt.Errorf("routing reply: store message: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO routing_replies
 (reply_id, parent_id, original_session_id, target_session_id, logical_agent_id, actor, interrupt, state,
  idempotency_key, request_hash, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, 'queued', ?, ?, ?, ?)`,
		id.String(), in.ParentID, in.TargetSessionID, in.TargetSessionID, in.LogicalAgentID, in.Actor, in.Interrupt,
		in.IdempotencyKey, hash, now, now); err != nil {
		return RoutingReply{}, false, fmt.Errorf("routing reply: queue: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return RoutingReply{}, false, err
	}
	reply, err = s.RoutingReply(ctx, id.String())
	return reply, true, err
}

func scanRoutingReplyWithHash(scan scanFunc, hash *string) (RoutingReply, error) {
	return scanRoutingReply(func(dest ...any) error { return scan(append(dest, hash)...) })
}

// RoutingReply reads one reply's delivery state.
func (s *Store) RoutingReply(ctx context.Context, replyID string) (RoutingReply, error) {
	r, err := scanRoutingReply(s.db.QueryRowContext(ctx,
		`SELECT `+routingReplyColumns+` FROM routing_replies WHERE reply_id=?`, replyID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RoutingReply{}, ErrRoutingReplyNotFound
	}
	return r, err
}

// RoutingReplyBody reads the single stored copy of the reply text.
func (s *Store) RoutingReplyBody(ctx context.Context, replyID string) (string, error) {
	var payload sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT m.payload FROM messages m
 JOIN routing_replies r ON r.reply_id = m.id WHERE r.reply_id=?`, replyID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRoutingReplyNotFound
	}
	if err != nil {
		return "", err
	}
	if !payload.Valid || payload.String == "" {
		return "", ErrRoutingReplyBodyPurged
	}
	var body struct {
		Body string `json:"body"`
	}
	if err := json.Unmarshal([]byte(payload.String), &body); err != nil {
		return "", fmt.Errorf("routing reply: decode body: %w", err)
	}
	return body.Body, nil
}

// QueuedRoutingReplies returns the replies waiting on sessionID, oldest first.
// A reply whose retry time has not arrived is not returned.
func (s *Store) QueuedRoutingReplies(ctx context.Context, sessionID string, now time.Time, limit int) ([]RoutingReply, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+routingReplyColumns+` FROM routing_replies
 WHERE target_session_id=? AND state='queued' AND (next_attempt_at IS NULL OR next_attempt_at <= ?)
 ORDER BY created_at ASC, reply_id ASC LIMIT ?`, sessionID, now.UTC().Format(time.RFC3339Nano), limit)
	if err != nil {
		return nil, err
	}
	return drainRoutingReplies(rows)
}

// RoutingRepliesInState lists replies in one state, oldest first. The repair
// sweep and startup recovery use it.
func (s *Store) RoutingRepliesInState(ctx context.Context, state RoutingReplyState, limit int) ([]RoutingReply, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+routingReplyColumns+` FROM routing_replies
 WHERE state=? ORDER BY created_at ASC, reply_id ASC LIMIT ?`, string(state), limit)
	if err != nil {
		return nil, err
	}
	return drainRoutingReplies(rows)
}

// RoutingRepliesForParent lists the replies to one message, oldest first.
func (s *Store) RoutingRepliesForParent(ctx context.Context, parentID string) ([]RoutingReply, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+routingReplyColumns+` FROM routing_replies
 WHERE parent_id=? ORDER BY created_at ASC, reply_id ASC`, parentID)
	if err != nil {
		return nil, err
	}
	return drainRoutingReplies(rows)
}

// drainRoutingReplies reads every row before returning: the store runs on one
// connection, so an open Rows while the caller queries again would deadlock.
func drainRoutingReplies(rows *sql.Rows) ([]RoutingReply, error) {
	defer rows.Close() //nolint:errcheck
	var out []RoutingReply
	for rows.Next() {
		r, err := scanRoutingReply(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimRoutingReply moves queued -> delivering and counts the attempt. It
// reports false when another drain already claimed (or settled) the reply, so
// the caller never injects the same reply twice.
func (s *Store) ClaimRoutingReply(ctx context.Context, replyID string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE routing_replies
 SET state='delivering', attempts=attempts+1, next_attempt_at=NULL, updated_at=?
 WHERE reply_id=? AND state='queued'`, routingNow(), replyID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RequeueRoutingReply puts a claimed (or queued) reply back, to be tried again
// no sooner than notBefore. A non-empty retarget re-points it at the session
// that now owns the actor. reason records why, for consumers.
func (s *Store) RequeueRoutingReply(ctx context.Context, replyID, retarget, reason string, notBefore time.Time) error {
	var next any
	if !notBefore.IsZero() {
		next = notBefore.UTC().Format(time.RFC3339Nano)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE routing_replies
 SET state='queued', reason=?, next_attempt_at=?, updated_at=?,
     target_session_id = CASE WHEN ? != '' THEN ? ELSE target_session_id END
 WHERE reply_id=? AND state IN ('queued','delivering')`,
		reason, next, routingNow(), retarget, retarget, replyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	return nil
}

// SettleRoutingReply records a terminal outcome. delivered stamps the message
// consumed and undeliverable stamps it canceled, so retention treats both as
// finished. Only a non-terminal reply can be settled.
func (s *Store) SettleRoutingReply(ctx context.Context, replyID string, state RoutingReplyState, reason, deliveredTo string) error {
	if !state.Terminal() {
		return fmt.Errorf("routing reply: %q is not a terminal state", state)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := routingNow()
	res, err := tx.ExecContext(ctx, `UPDATE routing_replies
 SET state=?, reason=?, delivered_to_session_id=?, updated_at=?, settled_at=?, next_attempt_at=NULL
 WHERE reply_id=? AND state IN ('queued','delivering')`, string(state), reason, deliveredTo, now, now, replyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	column := "consumed_at"
	if state == RoutingReplyUndeliverable {
		column = "canceled_at"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET `+column+`=? WHERE id=?`, now, replyID); err != nil {
		return err
	}
	return tx.Commit()
}
