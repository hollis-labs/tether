package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"
	"github.com/hollis-labs/tether/internal/events"
)

const messageEventBatch = 128

var errMessageContentPending = errors.New("mailbox content pending")

type messageEventRecord struct {
	deferred bool
	position int64
	at       time.Time
	teamRun  string
	events   []events.Event
}

// DrainMessageEvents atomically advances each source checkpoint with its event
// outbox rows. Crash/retry cannot skip or duplicate a committed observation.
// It never changes mailbox/delivery state or replays historical attention state.
func (s *Store) DrainMessageEvents(ctx context.Context, bus events.OutboxBus) (int, error) {
	var deliveryAfter, attentionAfter, retryAfter int64
	if err := s.db.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='delivery'`).Scan(&deliveryAfter); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='attention'`).Scan(&attentionAfter); err != nil {
		return 0, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='retry'`).Scan(&retryAfter); err != nil {
		return 0, err
	}
	transitions, err := s.DeliveryStore().Transitions(ctx, deliveryAfter, messageEventBatch)
	if err != nil {
		return 0, err
	}
	core := make([]messageEventRecord, 0, len(transitions))
	for _, tr := range transitions {
		record, err := s.messageTransition(ctx, tr)
		if errors.Is(err, errMessageContentPending) {
			record.deferred = true
		} else if err != nil {
			return 0, err
		}
		core = append(core, record)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,kind,at,payload_json,team_run_id FROM message_event_journal WHERE id>? ORDER BY id LIMIT ?`, attentionAfter, messageEventBatch)
	if err != nil {
		return 0, err
	}
	attention := []messageEventRecord{}
	for rows.Next() {
		var id int64
		var kind, at, raw, team string
		if err = rows.Scan(&id, &kind, &at, &raw, &team); err != nil {
			break
		}
		attention = append(attention, messageEventRecord{position: id, at: parseMsgTime(at), teamRun: team, events: []events.Event{messageMetadataEvent(kind, raw, parseMsgTime(at), team)}})
	}
	rowErr := rows.Err()
	_ = rows.Close()
	if err != nil {
		return 0, err
	}
	if rowErr != nil {
		return 0, rowErr
	}
	// A missing mailbox row (Enqueue/content commit gap or orphan) must not
	// stall unrelated mail. Keep its transition reference and retry independently.
	pendingRows, err := s.db.QueryContext(ctx, `SELECT transition_id FROM message_event_deferred ORDER BY CASE WHEN transition_id>? THEN 0 ELSE 1 END,transition_id LIMIT ?`, retryAfter, messageEventBatch)
	if err != nil {
		return 0, err
	}
	pendingIDs := []int64{}
	for pendingRows.Next() {
		var id int64
		if err = pendingRows.Scan(&id); err != nil {
			break
		}
		pendingIDs = append(pendingIDs, id)
	}
	pendingErr := pendingRows.Err()
	_ = pendingRows.Close()
	if err != nil {
		return 0, err
	}
	if pendingErr != nil {
		return 0, pendingErr
	}
	ready := []messageEventRecord{}
	for _, id := range pendingIDs {
		page, err := s.DeliveryStore().Transitions(ctx, id-1, 1)
		if err != nil {
			return 0, err
		}
		if len(page) != 1 || page[0].ID != id {
			return 0, fmt.Errorf("missing deferred transition %d", id)
		}
		record, err := s.messageTransition(ctx, page[0])
		if errors.Is(err, errMessageContentPending) {
			continue
		}
		if err != nil {
			return 0, err
		}
		ready = append(ready, record)
	}
	if len(core)+len(attention)+len(pendingIDs) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	// Another drainer may have committed while this one read. Check both source
	// positions under the write transaction instead of replaying stale snapshots.
	var d, a, r int64
	if err = tx.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='delivery'`).Scan(&d); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='attention'`).Scan(&a); err != nil {
		return 0, err
	}
	if err = tx.QueryRowContext(ctx, `SELECT position FROM message_event_checkpoints WHERE source='retry'`).Scan(&r); err != nil {
		return 0, err
	}
	if d != deliveryAfter || a != attentionAfter || r != retryAfter {
		return 0, nil
	}
	count := 0
	// Merge observation times while preserving each source's authoritative order,
	// including transition IDs whose timestamps need not increase.
	for len(core)+len(attention) > 0 {
		source := "delivery"
		var next messageEventRecord
		if len(attention) > 0 && (len(core) == 0 || attention[0].at.Before(core[0].at)) {
			source = "attention"
			next, attention = attention[0], attention[1:]
		} else {
			next, core = core[0], core[1:]
		}
		if next.deferred {
			if _, err = tx.ExecContext(ctx, `INSERT INTO message_event_deferred(transition_id) VALUES (?) ON CONFLICT DO NOTHING`, next.position); err != nil {
				return 0, err
			}
		}
		for _, ev := range next.events {
			if _, err = bus.PublishTx(ctx, tx, ev); err != nil {
				return 0, err
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE message_event_checkpoints SET position=? WHERE source=?`, next.position, source); err != nil {
			return 0, err
		}
		count++
	}
	for _, record := range ready {
		var exists int
		if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM message_event_deferred WHERE transition_id=?`, record.position).Scan(&exists); err != nil {
			return 0, err
		}
		if exists == 0 {
			continue
		}
		for _, ev := range record.events {
			if _, err = bus.PublishTx(ctx, tx, ev); err != nil {
				return 0, err
			}
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM message_event_deferred WHERE transition_id=?`, record.position); err != nil {
			return 0, err
		}
		count++
	}
	if len(pendingIDs) > 0 {
		if _, err = tx.ExecContext(ctx, `UPDATE message_event_checkpoints SET position=? WHERE source='retry'`, pendingIDs[len(pendingIDs)-1]); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	bus.KickOutbox()
	return count, nil
}

func (s *Store) messageTransition(ctx context.Context, tr delivery.Transition) (messageEventRecord, error) {
	record := messageEventRecord{position: tr.ID, at: tr.At}
	ds := s.DeliveryStore()
	msg, err := ds.GetMessage(ctx, tr.MessageID)
	if err != nil {
		return record, err
	}
	if strings.HasPrefix(string(msg.Kind), "stream.") {
		return record, nil
	}
	// GetDelivery performs lease expiry in the shipped library. Projection is
	// read-only: read the frozen recipient without advancing that state machine.
	var recipient string
	if err = s.db.QueryRowContext(ctx, `SELECT recipient_urn FROM messaging_deliveries WHERE id=?`, tr.DeliveryID).Scan(&recipient); err != nil {
		return record, err
	}
	messageID := string(msg.ID)
	if !msg.Group.IsZero() {
		messageID = msg.Metadata["tether_message_id"]
		if messageID == "" {
			return record, fmt.Errorf("group transition %d lacks canonical mailbox id", tr.ID)
		}
	}
	// Enqueue and mailbox content commit separately. Wait for the latter rather
	// than publishing a body reference for an uncommitted/orphaned mailbox row.
	var exists int
	if err = s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE id=? AND routing_staged=0`, messageID).Scan(&exists); err != nil {
		return record, err
	}
	if exists == 0 {
		return record, fmt.Errorf("transition %d: %w", tr.ID, errMessageContentPending)
	}
	attempt, generation, err := s.messageTransitionAttempt(ctx, tr)
	if err != nil {
		return record, err
	}

	team := ""
	if candidate := msg.Metadata["team_run"]; candidate != "" {
		err = s.db.QueryRowContext(ctx, `SELECT run_id FROM team_runs WHERE run_id=?`, candidate).Scan(&team)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return record, err
		}
	}
	urgency := ""
	switch v := msg.Metadata["urgency"]; v {
	case "very-low", "low", "normal", "high":
		urgency = v
	}
	groupSeq, _ := strconv.ParseInt(msg.Metadata["tether_group_seq"], 10, 64)
	ref := "/messages/" + url.PathEscape(messageID)
	if !msg.Group.IsZero() {
		ref = "/groups/" + url.PathEscape(msg.Group.URN()) + "/messages?since_seq=" + strconv.FormatInt(max(0, groupSeq-1), 10) + "&limit=1"
	}
	payload := map[string]any{"message_id": messageID, "from": msg.From.URN(), "to": []string{recipient}, "thread_id": msg.ThreadID, "in_reply_to": msg.InReplyTo, "msg_kind": msg.Kind, "urgency": urgency, "body_ref": ref, "body_bytes": len(msg.Payload), "content_type": msg.ContentType, "delivery": map[string]any{"state": tr.To, "attempt": attempt, "binding_generation": generation, "delivery_id": tr.DeliveryID, "attempt_id": tr.AttemptID}, "transition_id": tr.ID, "reason": tr.Reason, "delivery_message_id": tr.MessageID}
	if !msg.Group.IsZero() {
		payload["group"] = msg.Group.URN()
		payload["group_seq"] = groupSeq
	}
	kinds := []string{}
	switch tr.Reason {
	case delivery.TransitionCreated:
		kinds = []string{catalog.KindMessageSent, catalog.KindMessageQueued}
	case delivery.TransitionRedriven:
		kinds = []string{catalog.KindMessageRedriven, catalog.KindMessageQueued}
	case delivery.TransitionClaimed, delivery.TransitionConsumed, delivery.TransitionDeadlineExceeded:
	// Claim is not delivery evidence; consumed/deadline are handled by state below.
	case delivery.TransitionNacked, delivery.TransitionLeaseExpired:
		kinds = []string{catalog.KindMessageFailed}
	}
	if tr.To == delivery.DeliveryDeadLettered {
		kinds = append(kinds, catalog.KindMessageDeadLettered)
	}
	if tr.To == delivery.DeliveryDelivered {
		var consumed sql.NullString
		if err = s.db.QueryRowContext(ctx, `SELECT consumed_at FROM messages WHERE id=?`, messageID).Scan(&consumed); err != nil {
			return record, err
		}
		if !consumed.Valid {
			kinds = append(kinds, catalog.KindMessageConsumed)
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return record, err
	}
	for _, kind := range kinds {
		record.events = append(record.events, messageMetadataEvent(kind, string(raw), tr.At, team))
	}
	return record, nil
}
