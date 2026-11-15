package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Store is the event log's class B store.
type Store struct {
	all *store.Stores
	b   *store.Handle
	// Now is the log's clock (time.Now outside tests).
	Now func() time.Time
	// NewEventID mints an event id when a row has none (runtimeevents.NewEventID: "evt_" + 32 hex).
	NewEventID func() string
	// NewInputID mints a turn input id ("ti_" + 32 hex).
	NewInputID func() string
}

// New returns the store over the class handles.
func New(s *store.Stores) *Store {
	return &Store{all: s, b: s.B(), Now: time.Now, NewEventID: runtimeevents.NewEventID, NewInputID: newInputID}
}

// TurnOutputKind is the kind of a session turn output event (events.KindSessionTurnOutput in the
// old tree); HasPublishedProviderResult looks only at it.
const TurnOutputKind = "session.turn_output"

// ErrScopeRequired is returned for an event without a scope.
var ErrScopeRequired = errors.New("eventstore: event scope required")

func (s *Store) now() time.Time { return s.Now().UTC() }

// rebind rewrites ? placeholders for the querier's dialect.
func rebind(q gostore.Querier, query string) string { return q.Dialect().Rebind(query) }

func isPostgres(q gostore.Querier) bool { return q.Dialect().Name() == gostore.DriverPostgres }

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func clamp(limit, def, maxLimit int) int {
	if limit <= 0 {
		return def
	}
	return min(limit, maxLimit)
}

// insertSQL is the one INSERT every events write goes through. A repeat of a stored event id writes
// nothing and returns no row (the partial unique index idx_events_event_id is the conflict target on
// both dialects).
var insertSQL = `INSERT INTO events (` + strings.Join(insertColumns, ", ") + `) VALUES (` + placeholders(len(insertColumns)) +
	`) ON CONFLICT (event_id) WHERE event_id IS NOT NULL DO NOTHING RETURNING id`

// clockSkew bounds how far in the future a producer's at may be before the log's own time replaces
// it (the original is kept as origin_at), so a future date cannot evade at-based retention.
const clockSkew = 5 * time.Minute

// Insert writes one event in its own class B transaction. See InsertTx.
func (s *Store) Insert(ctx context.Context, r Row) (Row, error) {
	var out Row
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var err error
		out, _, err = s.InsertTx(ctx, tx, r, false)
		return err
	})
	return out, err
}

// InsertTx writes r inside the caller's transaction: a class B WriteTx, or a named joint (the outbox
// rows of the B-6 transactions). pending marks an outbox row. It applies the old store's write rules:
//   - a secret or unknown sensitivity is refused (secret events are never stored);
//   - retention class and policy come from the admission table (event_retention_admission, class A;
//     a read, so it is allowed inside a B transaction), never from the caller;
//   - a missing event id is minted, a zero at is the log's time, and an at more than clockSkew in the
//     future is replaced by the log's time with the original kept in OriginAt;
//   - recorded_at is the log's time; epoch and payload_digest are never written here.
//
// A repeat of a stored event id writes nothing and returns the stored row with existing=true.
func (s *Store) InsertTx(ctx context.Context, tx store.Tx, r Row, pending bool) (stored Row, existing bool, err error) {
	if r.Scope == "" {
		return Row{}, false, ErrScopeRequired
	}
	switch r.Sensitivity {
	case "", "public", "private":
	case "secret":
		return Row{}, false, fmt.Errorf("retention: secret events must not be stored")
	default:
		return Row{}, false, fmt.Errorf("retention: unknown event sensitivity")
	}
	r.RetentionClass, r.RetentionPolicyID = "", ""
	err = tx.QueryRowContext(ctx, rebind(tx, `SELECT a.retention_class, a.policy_id FROM event_retention_admission a
 JOIN event_retention_policies p ON p.id = a.policy_id WHERE a.kind = ?`), r.Kind).Scan(&r.RetentionClass, &r.RetentionPolicyID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, err
	}
	if r.EventID == "" {
		r.EventID = s.NewEventID()
	}
	logTime := s.now()
	at := r.At
	if at.IsZero() {
		at = logTime
	}
	r.OriginAt = time.Time{}
	if at.After(logTime.Add(clockSkew)) {
		r.OriginAt = at.UTC()
		at = logTime
	}
	r.At, r.RecordedAt, r.Pending = at.UTC(), logTime, pending
	r.Epoch, r.PayloadDigest = 0, ""
	err = tx.QueryRowContext(ctx, rebind(tx, insertSQL), r.insertValues()...).Scan(&r.Seq)
	if errors.Is(err, sql.ErrNoRows) {
		prev, err := scanRow(tx.QueryRowContext(ctx, rebind(tx, `SELECT `+readColumns+` FROM events WHERE event_id = ?`), r.EventID))
		if err != nil {
			return Row{}, false, fmt.Errorf("event %s already stored but unreadable: %w", r.EventID, err)
		}
		return prev, true, nil
	}
	if err != nil {
		return Row{}, false, err
	}
	return r, false, nil
}
