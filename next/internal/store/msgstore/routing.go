package msgstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/next/internal/store"
)

// RoutingReplyState is a routing reply's dispatch state.
type RoutingReplyState string

// Routing reply states.
const (
	RoutingReplyPending       RoutingReplyState = "pending"
	RoutingReplyQueued        RoutingReplyState = "queued"
	RoutingReplyDelivering    RoutingReplyState = "delivering"
	RoutingReplyDelivered     RoutingReplyState = "delivered"
	RoutingReplyUndeliverable RoutingReplyState = "undeliverable"
)

// Terminal reports whether the state is final.
func (s RoutingReplyState) Terminal() bool {
	return s == RoutingReplyDelivered || s == RoutingReplyUndeliverable
}

var (
	// ErrRoutingReplyNotFound is returned for an unknown reply id.
	ErrRoutingReplyNotFound = errors.New("routing reply: not found")
	// ErrRoutingReplyIdempotencyConflict is returned when an idempotency key is reused with a different reply.
	ErrRoutingReplyIdempotencyConflict = errors.New("routing reply: idempotency key reused with a different reply")
	// ErrRoutingReplyBodyPurged is returned when the reply's body was purged.
	ErrRoutingReplyBodyPurged = errors.New("routing reply: body was purged")
	// ErrRoutingReplyState is returned when the reply is not in the state the transition needs.
	ErrRoutingReplyState = errors.New("routing reply: not in the expected state")
)

// RoutingReplyAddress is the recipient of every routing reply message.
var RoutingReplyAddress = messaging.Address{Kind: messaging.KindService, Authority: "local", ID: "routing-reply"}

// RoutingReply is one reply routed back into a session.
type RoutingReply struct {
	ReplyID, ParentID, OriginalSessionID, TargetSessionID, DeliveredToSessionID string
	LogicalAgentID, Actor                                                       string
	Interrupt                                                                   bool
	State                                                                       RoutingReplyState
	Reason, Detail                                                              string
	Attempts                                                                    int
	NextAttemptAt                                                               *time.Time
	CreatedAt, UpdatedAt                                                        time.Time
	SettledAt                                                                   *time.Time
}

// NewRoutingReply is a reply to create. Pending creates it held until PromoteRoutingReply.
type NewRoutingReply struct {
	From                                      messaging.Address
	ParentID, ThreadID, Body, TargetSessionID string
	LogicalAgentID, Actor                     string
	Interrupt                                 bool
	IdempotencyKey                            string
	Metadata                                  map[string]string
	Pending                                   bool
}

func (n NewRoutingReply) hash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%v\x00%s", n.ParentID, n.Body, n.Interrupt, n.TargetSessionID)))
	return hex.EncodeToString(sum[:])
}

const routingReplyColumns = `reply_id, parent_id, original_session_id, target_session_id, delivered_to_session_id,
 logical_agent_id, actor, interrupt, state, reason, detail, attempts, next_attempt_at, created_at, updated_at, settled_at`

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func scanRoutingReply(scan scanFunc, extra ...any) (RoutingReply, error) {
	var r RoutingReply
	var state, created, updated string
	var interrupt int64
	var next, settled sql.NullString
	dest := append([]any{&r.ReplyID, &r.ParentID, &r.OriginalSessionID, &r.TargetSessionID, &r.DeliveredToSessionID,
		&r.LogicalAgentID, &r.Actor, &interrupt, &state, &r.Reason, &r.Detail, &r.Attempts, &next, &created, &updated, &settled}, extra...)
	if err := scan(dest...); err != nil {
		return RoutingReply{}, err
	}
	r.Interrupt, r.State = interrupt != 0, RoutingReplyState(state)
	r.CreatedAt, r.UpdatedAt = parseTime(created), parseTime(updated)
	r.NextAttemptAt, r.SettledAt = parseNullTime(next), parseNullTime(settled)
	return r, nil
}

// peekRoutingReply returns the reply stored under the same (parent, actor, idempotency key);
// a stored reply with a different request is ErrRoutingReplyIdempotencyConflict.
func peekRoutingReply(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, d func(string) string, in NewRoutingReply) (RoutingReply, bool, error) {
	if in.IdempotencyKey == "" {
		return RoutingReply{}, false, nil
	}
	var priorHash string
	prior, err := scanRoutingReply(q.QueryRowContext(ctx, d(`SELECT `+routingReplyColumns+`, request_hash FROM routing_replies
 WHERE parent_id = ? AND actor = ? AND idempotency_key = ?`), in.ParentID, in.Actor, in.IdempotencyKey).Scan, &priorHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RoutingReply{}, false, nil
	case err != nil:
		return RoutingReply{}, false, err
	case priorHash != in.hash():
		return RoutingReply{}, false, ErrRoutingReplyIdempotencyConflict
	}
	return prior, true, nil
}

// PeekRoutingReply returns an existing reply for the request's idempotency key.
func (s *Store) PeekRoutingReply(ctx context.Context, in NewRoutingReply) (RoutingReply, bool, error) {
	r := s.c.Reader()
	return peekRoutingReply(ctx, r, r.Dialect().Rebind, in)
}

// CreateRoutingReply stores the reply message (to RoutingReplyAddress) and its dispatch row in one
// transaction, queued or pending. A repeat with the same idempotency key returns the stored reply
// (created=false).
func (s *Store) CreateRoutingReply(ctx context.Context, in NewRoutingReply) (reply RoutingReply, created bool, err error) {
	if in.ParentID == "" || in.TargetSessionID == "" {
		return RoutingReply{}, false, fmt.Errorf("routing reply: parent and target session are required")
	}
	if _, err := messaging.ParseURN(in.From.URN()); err != nil {
		return RoutingReply{}, false, err
	}
	id, err := s.NewMessageID()
	if err != nil {
		return RoutingReply{}, false, err
	}
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
	initial := RoutingReplyQueued
	if in.Pending {
		initial = RoutingReplyPending
	}
	err = s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		prior, found, err := peekRoutingReply(ctx, tx, tx.Dialect().Rebind, in)
		if err != nil || found {
			reply = prior
			return err
		}
		if err := LockMessageSeq(ctx, tx); err != nil {
			return err
		}
		now := stamp(s.now())
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO messages
 (id, kind, channel, from_urn, to_urn, thread_id, in_reply_to, payload, content_type, metadata, created_at)
 VALUES (?, ?, '', ?, ?, ?, ?, ?, 'application/json', ?, ?)`),
			id, string(messaging.MsgKindResponse), in.From.URN(), RoutingReplyAddress.URN(), thread, in.ParentID, string(body), string(metaJSON), now); err != nil {
			return fmt.Errorf("routing reply: store message: %w", err)
		}
		if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO routing_replies
 (reply_id, parent_id, original_session_id, target_session_id, logical_agent_id, actor, interrupt, state, idempotency_key, request_hash, created_at, updated_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			id, in.ParentID, in.TargetSessionID, in.TargetSessionID, in.LogicalAgentID, in.Actor, boolInt(in.Interrupt),
			string(initial), in.IdempotencyKey, in.hash(), now, now); err != nil {
			return fmt.Errorf("routing reply: queue: %w", err)
		}
		created = true
		return nil
	})
	if err != nil || !created {
		return reply, false, err
	}
	reply, err = s.RoutingReply(ctx, id)
	return reply, true, err
}

// RoutingReply returns one reply.
func (s *Store) RoutingReply(ctx context.Context, replyID string) (RoutingReply, error) {
	r := s.c.Reader()
	reply, err := scanRoutingReply(r.QueryRowContext(ctx, rebind(r, `SELECT `+routingReplyColumns+` FROM routing_replies WHERE reply_id = ?`), replyID).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return RoutingReply{}, ErrRoutingReplyNotFound
	}
	return reply, err
}

// RoutingReplyBody returns the reply's text (ErrRoutingReplyBodyPurged once purged).
func (s *Store) RoutingReplyBody(ctx context.Context, replyID string) (string, error) {
	r := s.c.Reader()
	var payload sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT m.payload FROM messages m JOIN routing_replies rr ON rr.reply_id = m.id WHERE rr.reply_id = ?`), replyID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRoutingReplyNotFound
	}
	if err != nil {
		return "", err
	}
	if payload.String == "" {
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

func (s *Store) replies(ctx context.Context, where string, args ...any) ([]RoutingReply, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT `+routingReplyColumns+` FROM routing_replies WHERE `+where), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RoutingReply
	for rows.Next() {
		reply, err := scanRoutingReply(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, reply)
	}
	return out, rows.Err()
}

// QueuedRoutingReplies returns a session's queued replies, interrupts first, then oldest (limit
// default 50).
func (s *Store) QueuedRoutingReplies(ctx context.Context, sessionID string, limit int) ([]RoutingReply, error) {
	if limit <= 0 {
		limit = 50
	}
	return s.replies(ctx, `target_session_id = ? AND state = 'queued' ORDER BY interrupt DESC, created_at ASC, reply_id ASC LIMIT ?`, sessionID, limit)
}

// RoutingRepliesInState returns replies in a state, oldest first (limit default 200).
func (s *Store) RoutingRepliesInState(ctx context.Context, state RoutingReplyState, limit int) ([]RoutingReply, error) {
	if limit <= 0 {
		limit = 200
	}
	return s.replies(ctx, `state = ? ORDER BY created_at ASC, reply_id ASC LIMIT ?`, string(state), limit)
}

// RoutingRepliesForParent returns every reply to a parent message, oldest first.
func (s *Store) RoutingRepliesForParent(ctx context.Context, parentID string) ([]RoutingReply, error) {
	return s.replies(ctx, `parent_id = ? ORDER BY created_at ASC, reply_id ASC`, parentID)
}

// SessionHasOpenRoutingReplies reports whether a session has a queued or delivering reply.
func (s *Store) SessionHasOpenRoutingReplies(ctx context.Context, sessionID string) (bool, error) {
	r := s.c.Reader()
	var one int
	err := r.QueryRowContext(ctx, rebind(r, `SELECT 1 FROM routing_replies WHERE target_session_id = ? AND state IN ('queued','delivering') LIMIT 1`), sessionID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// SessionsWithQueuedRoutingReplies lists the sessions that have a queued reply.
func (s *Store) SessionsWithQueuedRoutingReplies(ctx context.Context) ([]string, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, `SELECT DISTINCT target_session_id FROM routing_replies WHERE state = 'queued' ORDER BY target_session_id`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) transition(ctx context.Context, q string, args ...any) error {
	n, err := s.execC(ctx, q, args...)
	if err != nil {
		return err
	}
	if n != 1 {
		return ErrRoutingReplyState
	}
	return nil
}

// PromoteRoutingReply moves a pending reply to queued.
func (s *Store) PromoteRoutingReply(ctx context.Context, replyID string) error {
	return s.transition(ctx, `UPDATE routing_replies SET state = 'queued', updated_at = ? WHERE reply_id = ? AND state = 'pending'`, stamp(s.now()), replyID)
}

// DiscardPendingRoutingReply deletes a pending reply and its message.
func (s *Store) DiscardPendingRoutingReply(ctx context.Context, replyID string) error {
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `DELETE FROM routing_replies WHERE reply_id = ? AND state = 'pending'`), replyID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrRoutingReplyState
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `DELETE FROM messages WHERE id = ?`), replyID)
		return err
	})
}

// RoutingReplyDetailMax bounds a stored detail.
const RoutingReplyDetailMax = 256

// BoundRoutingDetail collapses whitespace and cuts a detail to RoutingReplyDetailMax bytes on a
// rune boundary.
func BoundRoutingDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) <= RoutingReplyDetailMax {
		return detail
	}
	end := RoutingReplyDetailMax - len("...")
	for end > 0 && !utf8.RuneStart(detail[end]) {
		end--
	}
	return detail[:end] + "..."
}

// ClaimRoutingReply moves a queued reply to delivering and counts the attempt.
func (s *Store) ClaimRoutingReply(ctx context.Context, replyID string) (bool, error) {
	n, err := s.execC(ctx, `UPDATE routing_replies SET state = 'delivering', attempts = attempts + 1, next_attempt_at = NULL, updated_at = ?
 WHERE reply_id = ? AND state = 'queued'`, stamp(s.now()), replyID)
	return n == 1, err
}

// RoutingReplyRequeue puts a reply back in the queue. IfUnchanged makes it conditional on the
// snapshot's state and updated_at.
type RoutingReplyRequeue struct {
	Retarget       string
	Reason, Detail string
	NotBefore      time.Time
	RefundAttempt  bool
	IfUnchanged    *RoutingReply
}

func unchangedSince(snapshot *RoutingReply) (guarded int64, state, updatedAt string) {
	if snapshot == nil {
		return 0, "", ""
	}
	return 1, string(snapshot.State), stamp(snapshot.UpdatedAt)
}

// RequeueRoutingReply requeues a queued or delivering reply.
func (s *Store) RequeueRoutingReply(ctx context.Context, replyID string, in RoutingReplyRequeue) error {
	var next any
	if !in.NotBefore.IsZero() {
		next = stamp(in.NotBefore)
	}
	guarded, gState, gUpdated := unchangedSince(in.IfUnchanged)
	return s.transition(ctx, `UPDATE routing_replies
 SET state = 'queued', reason = ?, detail = ?, next_attempt_at = ?, updated_at = ?,
     target_session_id = CASE WHEN ? <> '' THEN ? ELSE target_session_id END,
     attempts = CASE WHEN ? = 1 AND attempts > 0 THEN attempts - 1 ELSE attempts END
 WHERE reply_id = ? AND state IN ('queued','delivering') AND (? = 0 OR (state = ? AND updated_at = ?))`,
		in.Reason, BoundRoutingDetail(in.Detail), next, stamp(s.now()), in.Retarget, in.Retarget, boolInt(in.RefundAttempt), replyID,
		guarded, gState, gUpdated)
}

// RoutingReplySettlement settles a reply in a terminal state.
type RoutingReplySettlement struct {
	State          RoutingReplyState
	Reason, Detail string
	DeliveredTo    string
	IfUnchanged    *RoutingReply
}

// SettleRoutingReply settles a reply and stamps its message consumed (delivered) or canceled
// (undeliverable) in one transaction.
func (s *Store) SettleRoutingReply(ctx context.Context, replyID string, in RoutingReplySettlement) error {
	if !in.State.Terminal() {
		return fmt.Errorf("routing reply: %q is not a terminal state", in.State)
	}
	column := "consumed_at"
	if in.State == RoutingReplyUndeliverable {
		column = "canceled_at"
	}
	now := stamp(s.now())
	guarded, gState, gUpdated := unchangedSince(in.IfUnchanged)
	return s.c.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		res, err := tx.ExecContext(ctx, rebind(tx, `UPDATE routing_replies
 SET state = ?, reason = ?, detail = ?, delivered_to_session_id = ?, updated_at = ?, settled_at = ?, next_attempt_at = NULL
 WHERE reply_id = ? AND state IN ('pending','queued','delivering') AND (? = 0 OR (state = ? AND updated_at = ?))`),
			string(in.State), in.Reason, BoundRoutingDetail(in.Detail), in.DeliveredTo, now, now, replyID, guarded, gState, gUpdated)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return ErrRoutingReplyState
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `UPDATE messages SET `+column+` = ? WHERE id = ?`), now, replyID)
		return err
	})
}

// RoutingReplySender returns the reply's sender and metadata.
func (s *Store) RoutingReplySender(ctx context.Context, replyID string) (messaging.Address, map[string]string, error) {
	r := s.c.Reader()
	var from string
	var raw sql.NullString
	err := r.QueryRowContext(ctx, rebind(r, `SELECT m.from_urn, m.metadata FROM messages m JOIN routing_replies rr ON rr.reply_id = m.id WHERE rr.reply_id = ?`), replyID).Scan(&from, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return messaging.Address{}, nil, ErrRoutingReplyNotFound
	}
	if err != nil {
		return messaging.Address{}, nil, err
	}
	addr, err := messaging.ParseURN(from)
	if err != nil {
		return messaging.Address{}, nil, err
	}
	meta := map[string]string{}
	if raw.String != "" {
		if err := json.Unmarshal([]byte(raw.String), &meta); err != nil {
			return addr, nil, fmt.Errorf("routing reply: decode metadata: %w", err)
		}
	}
	return addr, meta, nil
}
