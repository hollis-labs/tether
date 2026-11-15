package msgstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// This file is the storage half of the old message_events.go and message_events_attempt.go: the
// message lifecycle drain (B-6, the allowlisted joint "DrainMessageEvents"). The old
// DrainMessageEvents read three sources (go-messaging's delivery transitions, the attention journal
// the messages triggers write, and the deferred transitions), built events, and committed them with
// the source checkpoints in one transaction. Building the events (kinds, payloads, the envelope and
// the outbox kick) is the event slice's (S8, with message_events_payload.go); the store gives it the
// reads and the commit:
//
//	cp := MessageEventCheckpoints; DeliveryTransitions(cp.Delivery); TransitionFacts for each;
//	AttentionJournal(cp.Attention); DeferredTransitionIDs(cp.Retry) and DeliveryTransition for each;
//	CommitMessageEvents(batch) as the joint; then kick the outbox.
//
// The library reads run before the joint, never inside it (the library handle is refused there).
// CommitMessageEvents re-reads the checkpoints under a row lock and commits nothing when another
// drainer moved them since cp was read, as the old drain did.

// MessageEventBatch is the drain's per-source batch (the old messageEventBatch).
const MessageEventBatch = 128

// ErrMessageContentPending is returned by TransitionFacts when the transition's mailbox row is not
// committed (Enqueue and the mailbox content commit separately): the drain defers the transition
// rather than publish a body reference for an uncommitted or orphaned row.
var ErrMessageContentPending = errors.New("mailbox content pending")

// MessageEventCheckpoints are the drain's three source positions.
type MessageEventCheckpoints struct {
	Delivery, Attention, Retry int64
}

// MessageEventCheckpoints reads the drain's positions.
func (s *Store) MessageEventCheckpoints(ctx context.Context) (MessageEventCheckpoints, error) {
	return readCheckpoints(ctx, s.c.Reader(), "")
}

func readCheckpoints(ctx context.Context, q gostore.Querier, lock string) (MessageEventCheckpoints, error) {
	var cp MessageEventCheckpoints
	rows, err := q.QueryContext(ctx, `SELECT source, position FROM message_event_checkpoints WHERE source IN ('delivery','attention','retry') ORDER BY source`+lock)
	if err != nil {
		return cp, err
	}
	defer func() { _ = rows.Close() }()
	seen := 0
	for rows.Next() {
		var source string
		var pos int64
		if err := rows.Scan(&source, &pos); err != nil {
			return cp, err
		}
		switch source {
		case "delivery":
			cp.Delivery = pos
		case "attention":
			cp.Attention = pos
		case "retry":
			cp.Retry = pos
		}
		seen++
	}
	if err := rows.Err(); err != nil {
		return cp, err
	}
	if seen != 3 {
		return cp, fmt.Errorf("message event checkpoints: %d of 3 sources present: %w", seen, sql.ErrNoRows)
	}
	return cp, nil
}

// DeliveryTransitions returns up to limit library transitions after the position (library handle).
func (s *Store) DeliveryTransitions(ctx context.Context, after int64, limit int) ([]delivery.Transition, error) {
	ds, err := s.DeliveryStore(ctx)
	if err != nil {
		return nil, err
	}
	return ds.Transitions(ctx, after, limit)
}

// DeliveryTransition returns one library transition by id (a deferred transition's retry).
func (s *Store) DeliveryTransition(ctx context.Context, id int64) (delivery.Transition, error) {
	page, err := s.DeliveryTransitions(ctx, id-1, 1)
	if err != nil {
		return delivery.Transition{}, err
	}
	if len(page) != 1 || page[0].ID != id {
		return delivery.Transition{}, fmt.Errorf("missing deferred transition %d", id)
	}
	return page[0], nil
}

// TransitionFacts are the rows the event slice builds a transition's lifecycle events from.
type TransitionFacts struct {
	Transition delivery.Transition
	// Message is the library's message. A stream.* kind stops here: the old drain published nothing
	// for it, and the other fields are left empty.
	Message delivery.Message
	Stream  bool
	// Recipient is the delivery's frozen recipient URN (read without the library's GetDelivery, which
	// would advance lease expiry).
	Recipient string
	// MailboxID is Tether's message id: the library message's own id, or for a group post its
	// tether_message_id metadata.
	MailboxID string
	// Attempt is the attempt ordinal at the transition (claims after it subtracted from the durable
	// count) and BindingGeneration the attempt's binding generation (0 without an attempt).
	Attempt           int
	BindingGeneration int64
	// TeamRun is the metadata's team_run when that run exists, else "".
	TeamRun string
	// Consumed reports the mailbox row's consumed_at is set.
	Consumed bool
}

// TransitionFacts reads what the drain needs about one transition: the library message, then (for a
// non-stream kind) the delivery's recipient and attempt from the library tables, and the mailbox row,
// team run and consumed state from Tether's. ErrMessageContentPending when the mailbox row (not
// staged) is not there yet. It changes nothing.
func (s *Store) TransitionFacts(ctx context.Context, tr delivery.Transition) (TransitionFacts, error) {
	f := TransitionFacts{Transition: tr}
	db, err := s.all.LibraryDB(ctx)
	if err != nil {
		return f, err
	}
	ds := delivery.NewSQLiteStore(db)
	if f.Message, err = ds.GetMessage(ctx, tr.MessageID); err != nil {
		return f, err
	}
	if strings.HasPrefix(string(f.Message.Kind), "stream.") {
		f.Stream = true
		return f, nil
	}
	if err := db.QueryRowContext(ctx, `SELECT recipient_urn FROM messaging_deliveries WHERE id = ?`, tr.DeliveryID).Scan(&f.Recipient); err != nil {
		return f, err
	}
	f.MailboxID = string(f.Message.ID)
	if !f.Message.Group.IsZero() {
		if f.MailboxID = f.Message.Metadata["tether_message_id"]; f.MailboxID == "" {
			return f, fmt.Errorf("group transition %d lacks canonical mailbox id", tr.ID)
		}
	}
	r := s.c.Reader()
	var consumed sql.NullString
	err = r.QueryRowContext(ctx, rebind(r, `SELECT consumed_at FROM messages WHERE id = ? AND routing_staged = 0`), f.MailboxID).Scan(&consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return f, fmt.Errorf("transition %d: %w", tr.ID, ErrMessageContentPending)
	}
	if err != nil {
		return f, err
	}
	f.Consumed = consumed.Valid
	if f.Attempt, f.BindingGeneration, err = transitionAttempt(ctx, db, ds, tr); err != nil {
		return f, err
	}
	if candidate := f.Message.Metadata["team_run"]; candidate != "" {
		err := r.QueryRowContext(ctx, rebind(r, `SELECT run_id FROM team_runs WHERE run_id = ?`), candidate).Scan(&f.TeamRun)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return f, err
		}
	}
	return f, nil
}

// transitionAttempt is the old messageTransitionAttempt: the ordinal follows the committed claim
// sequence, not timestamps (the library allows nonmonotonic transition times), so one read subtracts
// later claims from the durable attempt count, pre-log attempts included. Library tables only, read.
func transitionAttempt(ctx context.Context, db *sql.DB, ds *delivery.SQLiteStore, tr delivery.Transition) (int, int64, error) {
	var number int
	err := db.QueryRowContext(ctx, `SELECT attempt_count - (SELECT count(*) FROM messaging_transitions t WHERE t.delivery_id = d.id AND t.reason = ? AND t.id > ?)
 FROM messaging_deliveries d WHERE d.id = ?`, delivery.TransitionClaimed, tr.ID, tr.DeliveryID).Scan(&number)
	if err != nil {
		return 0, 0, err
	}
	if tr.AttemptID == "" {
		return number, 0, nil
	}
	attempts, err := ds.Attempts(ctx, tr.DeliveryID)
	if err != nil {
		return 0, 0, err
	}
	for _, a := range attempts {
		if a.ID == tr.AttemptID {
			return number, a.BindingGeneration, nil
		}
	}
	return 0, 0, delivery.ErrNotFound
}

// JournalRow is one message_event_journal row (the attention source, written by the messages
// triggers and RecordMessageWake).
type JournalRow struct {
	ID          int64
	Kind        string
	At          time.Time
	PayloadJSON string
	TeamRunID   string
}

// AttentionJournal returns up to limit journal rows after the position, in id order.
func (s *Store) AttentionJournal(ctx context.Context, after int64, limit int) ([]JournalRow, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT id, kind, at, payload_json, team_run_id FROM message_event_journal WHERE id > ? ORDER BY id LIMIT ?`), after, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []JournalRow
	for rows.Next() {
		var j JournalRow
		var at string
		if err := rows.Scan(&j.ID, &j.Kind, &at, &j.PayloadJSON, &j.TeamRunID); err != nil {
			return nil, err
		}
		j.At = parseTime(at)
		out = append(out, j)
	}
	return out, rows.Err()
}

// DeferredTransitionIDs returns up to limit deferred transition ids, those after retryAfter first
// (then the wrap-around), each group in id order: a missing mailbox row must not stall other mail, so
// each deferred transition is retried on its own.
func (s *Store) DeferredTransitionIDs(ctx context.Context, retryAfter int64, limit int) ([]int64, error) {
	r := s.c.Reader()
	rows, err := r.QueryContext(ctx, rebind(r, `SELECT transition_id FROM message_event_deferred
 ORDER BY CASE WHEN transition_id > ? THEN 0 ELSE 1 END, transition_id LIMIT ?`), retryAfter, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// DrainRecord is one source observation with the event rows the event slice built for it. Position
// is the source's id (a transition or journal id). Deferred marks a delivery transition whose mailbox
// row was pending: it is parked in message_event_deferred (its Events are usually empty).
type DrainRecord struct {
	Position int64
	At       time.Time
	Deferred bool
	Events   []eventstore.Row
}

// DrainBatch is one drain's commit: the checkpoints it read (From), the delivery and attention
// records in each source's order, the deferred transitions now ready, and every deferred id it
// looked at (Pending, which moves the retry checkpoint past the last).
type DrainBatch struct {
	From      MessageEventCheckpoints
	Delivery  []DrainRecord
	Attention []DrainRecord
	Ready     []DrainRecord
	Pending   []int64
}

// CommitMessageEvents commits one drain as the joint "DrainMessageEvents" (class C checkpoints and
// deferred rows, class B outbox events, until the S8 redesign) and returns how many observations it
// committed. It locks the checkpoint rows (FOR UPDATE on Postgres; SQLite's writer already
// serializes) and commits nothing, returning 0, when they moved since From: another drainer
// committed first, so a stale snapshot is never replayed. Delivery and attention records are merged
// by observation time (attention first only when strictly earlier), keeping each source's own order;
// each advances its source checkpoint. A ready deferred record publishes only if this call removed
// its deferred row. The caller kicks the outbox after a commit.
func (s *Store) CommitMessageEvents(ctx context.Context, b DrainBatch, events *eventstore.Store) (int, error) {
	if len(b.Delivery)+len(b.Attention)+len(b.Pending) == 0 {
		return 0, nil
	}
	if events == nil {
		return 0, fmt.Errorf("commit message events: an event store is required")
	}
	count := 0
	err := s.all.Joint(ctx, "DrainMessageEvents", func(ctx context.Context, tx store.Tx) error {
		count = 0
		lock := ""
		if tx.Dialect().Name() == gostore.DriverPostgres {
			lock = " FOR UPDATE"
		}
		cp, err := readCheckpoints(ctx, tx, lock)
		if err != nil {
			return err
		}
		if cp != b.From {
			return nil
		}
		publish := func(rows []eventstore.Row) error {
			for _, r := range rows {
				if _, _, err := events.InsertTx(ctx, tx, r, true); err != nil {
					return err
				}
			}
			return nil
		}
		core, attention := b.Delivery, b.Attention
		for len(core)+len(attention) > 0 {
			source := "delivery"
			var next DrainRecord
			if len(attention) > 0 && (len(core) == 0 || attention[0].At.Before(core[0].At)) {
				source, next, attention = "attention", attention[0], attention[1:]
			} else {
				next, core = core[0], core[1:]
			}
			if next.Deferred {
				if _, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO message_event_deferred (transition_id) VALUES (?) ON CONFLICT DO NOTHING`), next.Position); err != nil {
					return err
				}
			}
			if err := publish(next.Events); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE message_event_checkpoints SET position = ? WHERE source = ?`), next.Position, source); err != nil {
				return err
			}
			count++
		}
		for _, record := range b.Ready {
			res, err := tx.ExecContext(ctx, rebind(tx, `DELETE FROM message_event_deferred WHERE transition_id = ?`), record.Position)
			if err != nil {
				return err
			}
			if n, err := res.RowsAffected(); err != nil {
				return err
			} else if n == 0 {
				continue // another drain already published it
			}
			if err := publish(record.Events); err != nil {
				return err
			}
			count++
		}
		if len(b.Pending) > 0 {
			if _, err := tx.ExecContext(ctx, rebind(tx, `UPDATE message_event_checkpoints SET position = ? WHERE source = 'retry'`), b.Pending[len(b.Pending)-1]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}
