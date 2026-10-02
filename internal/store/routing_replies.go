package store

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

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/go-messaging"
)

// RoutingReplyState is where a reply to a routed message stands. The dispatcher
// moves it queued -> delivering -> delivered, or to undeliverable with a reason;
// it never deletes the row (ADR 0049 s1.6). pending is the one state the
// dispatcher never drains: it reserves an interrupting reply while the running
// turn is canceled, and becomes queued (or is discarded, if the interrupt was
// refused and the reply therefore never accepted).
type RoutingReplyState string

const (
	RoutingReplyPending       RoutingReplyState = "pending"
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
	// ErrRoutingReplyNotMailbox is a mailbox verb (cancel, consume, read, archive)
	// aimed at a reply. Its delivery state lives in routing_replies only.
	ErrRoutingReplyNotMailbox = errors.New("routing replies are not mailbox items")
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
	Detail               string
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
	// Pending stores the reply as pending rather than queued; see
	// PromoteRoutingReply and DiscardPendingRoutingReply.
	Pending bool
}

func (n NewRoutingReply) hash() string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%v\x00%s", n.ParentID, n.Body, n.Interrupt, n.TargetSessionID)))
	return hex.EncodeToString(sum[:])
}

const routingReplyColumns = `reply_id, parent_id, original_session_id, target_session_id, delivered_to_session_id,
 logical_agent_id, actor, interrupt, state, reason, detail, attempts, next_attempt_at, created_at, updated_at, settled_at`

func scanRoutingReply(scan scanFunc) (RoutingReply, error) {
	var (
		r                      RoutingReply
		state                  string
		created, updated       string
		nextAttempt, settledAt sql.NullString
	)
	if err := scan(&r.ReplyID, &r.ParentID, &r.OriginalSessionID, &r.TargetSessionID, &r.DeliveredToSessionID,
		&r.LogicalAgentID, &r.Actor, &r.Interrupt, &state, &r.Reason, &r.Detail, &r.Attempts, &nextAttempt, &created, &updated, &settledAt); err != nil {
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

	if prior, found, err := peekRoutingReply(ctx, tx.QueryRowContext, in); err != nil || found {
		return prior, false, err
	}
	hash := in.hash()
	initial := RoutingReplyQueued
	if in.Pending {
		initial = RoutingReplyPending
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
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id.String(), in.ParentID, in.TargetSessionID, in.TargetSessionID, in.LogicalAgentID, in.Actor, in.Interrupt,
		string(initial), in.IdempotencyKey, hash, now, now); err != nil {
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

// PeekRoutingReply reports whether in's idempotency key already names a reply.
// found with a nil error is the earlier reply, unchanged; a different reply
// under the same key is ErrRoutingReplyIdempotencyConflict. It lets a caller
// check before doing something it must not repeat, such as interrupting a turn.
func (s *Store) PeekRoutingReply(ctx context.Context, in NewRoutingReply) (RoutingReply, bool, error) {
	return peekRoutingReply(ctx, s.db.QueryRowContext, in)
}

func peekRoutingReply(ctx context.Context, queryRow func(context.Context, string, ...any) *sql.Row, in NewRoutingReply) (RoutingReply, bool, error) {
	if in.IdempotencyKey == "" {
		return RoutingReply{}, false, nil
	}
	var priorHash string
	row := queryRow(ctx, `SELECT `+routingReplyColumns+`, request_hash FROM routing_replies
 WHERE parent_id=? AND actor=? AND idempotency_key=?`, in.ParentID, in.Actor, in.IdempotencyKey)
	prior, err := scanRoutingReplyWithHash(row.Scan, &priorHash)
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

// QueuedRoutingReplies returns the replies waiting on sessionID in delivery
// order: an interrupting reply first, then oldest first. It includes a reply whose
// retry time has not arrived; the caller delivers only the head, so a younger
// reply that happens to be due never jumps an older one that is backing off.
func (s *Store) QueuedRoutingReplies(ctx context.Context, sessionID string, limit int) ([]RoutingReply, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+routingReplyColumns+` FROM routing_replies
 WHERE target_session_id=? AND state='queued'
 ORDER BY interrupt DESC, created_at ASC, reply_id ASC LIMIT ?`, sessionID, limit)
	if err != nil {
		return nil, err
	}
	return drainRoutingReplies(rows)
}

// SessionsWithQueuedRoutingReplies names every session that has a reply waiting.
// The repair sweep runs it every few seconds, so it reads only the open-reply
// index, never the settled history.
func (s *Store) SessionsWithQueuedRoutingReplies(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT target_session_id FROM routing_replies WHERE state='queued'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
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

// PromoteRoutingReply makes a pending reply deliverable. It reports
// ErrRoutingReplyState if the reply is no longer pending.
func (s *Store) PromoteRoutingReply(ctx context.Context, replyID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE routing_replies SET state='queued', updated_at=?
 WHERE reply_id=? AND state='pending'`, routingNow(), replyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	return nil
}

// DiscardPendingRoutingReply removes a reply that was reserved but never
// accepted (its interrupt was refused), body and idempotency key included. It is
// the only deletion in this table, and only a pending row, which no consumer was
// ever told was queued, can be discarded.
func (s *Store) DiscardPendingRoutingReply(ctx context.Context, replyID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `DELETE FROM routing_replies WHERE reply_id=? AND state='pending'`, replyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id=?`, replyID); err != nil {
		return err
	}
	return tx.Commit()
}

// RoutingReplyDetailMax bounds the free-text detail stored with, and published
// about, a reply: a subprocess runtime's error carries up to 2 KB of stderr, and
// nothing a model process printed belongs in an event or a delivery view.
const RoutingReplyDetailMax = 256

// BoundRoutingDetail returns detail on one line, cut to RoutingReplyDetailMax
// bytes at a rune boundary.
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

// RoutingRepliesInState lists replies in one state, oldest first. Startup
// recovery and the stale-row check use it.
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

// RoutingReplyRequeue puts a reply back in the queue.
type RoutingReplyRequeue struct {
	// Retarget re-points the reply at the session that now owns the actor;
	// empty keeps the current target.
	Retarget string
	// Reason is a stable code consumers can key on; Detail is free text.
	Reason, Detail string
	// NotBefore is the earliest retry; zero means as soon as the session is idle.
	NotBefore time.Time
	// RefundAttempt gives back the attempt the claim counted, for a wait that is
	// not a failure (a runtime that rejects mid-turn input).
	RefundAttempt bool
	// IfUnchanged, when set, makes the requeue conditional on the reply still being
	// exactly as this snapshot read it (same state, not touched since). A caller
	// acting on a snapshot, such as the stale-row sweep, sets it so it cannot
	// overwrite a transition that happened after the read.
	IfUnchanged *RoutingReply
}

// unchangedSince is the arguments of the statements' trailing condition
// "(? = 0 OR (state = ? AND updated_at = ?))" for IfUnchanged: every transition
// stamps updated_at, so state plus updated_at identifies the exact row version the
// snapshot saw. With no snapshot the condition is vacuous.
func unchangedSince(snapshot *RoutingReply) (guarded int, state, updatedAt string) {
	if snapshot == nil {
		return 0, "", ""
	}
	return 1, string(snapshot.State), snapshot.UpdatedAt.UTC().Format(time.RFC3339Nano)
}

// RequeueRoutingReply puts a claimed (or queued) reply back, to be tried again.
func (s *Store) RequeueRoutingReply(ctx context.Context, replyID string, in RoutingReplyRequeue) error {
	var next any
	if !in.NotBefore.IsZero() {
		next = in.NotBefore.UTC().Format(time.RFC3339Nano)
	}
	guarded, guardState, guardUpdated := unchangedSince(in.IfUnchanged)
	res, err := s.db.ExecContext(ctx, `UPDATE routing_replies
 SET state='queued', reason=?, detail=?, next_attempt_at=?, updated_at=?,
     target_session_id = CASE WHEN ? != '' THEN ? ELSE target_session_id END,
     attempts = CASE WHEN ? AND attempts > 0 THEN attempts - 1 ELSE attempts END
 WHERE reply_id=? AND state IN ('queued','delivering')
   AND (? = 0 OR (state = ? AND updated_at = ?))`,
		in.Reason, BoundRoutingDetail(in.Detail), next, routingNow(), in.Retarget, in.Retarget, in.RefundAttempt, replyID,
		guarded, guardState, guardUpdated)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	return nil
}

// RoutingReplySettlement is a terminal outcome.
type RoutingReplySettlement struct {
	State          RoutingReplyState
	Reason, Detail string
	// DeliveredTo is the session that received the reply (delivered only).
	DeliveredTo string
	// IfUnchanged, when set, makes the settlement conditional on the reply still
	// being exactly as this snapshot read it; see RoutingReplyRequeue.IfUnchanged.
	IfUnchanged *RoutingReply
}

// SettleRoutingReply records a terminal outcome. delivered stamps the message
// consumed and undeliverable stamps it canceled, so retention treats both as
// finished. Only a non-terminal reply can be settled.
func (s *Store) SettleRoutingReply(ctx context.Context, replyID string, in RoutingReplySettlement) error {
	if !in.State.Terminal() {
		return fmt.Errorf("routing reply: %q is not a terminal state", in.State)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := routingNow()
	guarded, guardState, guardUpdated := unchangedSince(in.IfUnchanged)
	res, err := tx.ExecContext(ctx, `UPDATE routing_replies
 SET state=?, reason=?, detail=?, delivered_to_session_id=?, updated_at=?, settled_at=?, next_attempt_at=NULL
 WHERE reply_id=? AND state IN ('pending','queued','delivering')
   AND (? = 0 OR (state = ? AND updated_at = ?))`,
		string(in.State), in.Reason, BoundRoutingDetail(in.Detail), in.DeliveredTo, now, now, replyID,
		guarded, guardState, guardUpdated)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrRoutingReplyState
	}
	column := "consumed_at"
	if in.State == RoutingReplyUndeliverable {
		column = "canceled_at"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET `+column+`=? WHERE id=?`, now, replyID); err != nil {
		return err
	}
	return tx.Commit()
}
