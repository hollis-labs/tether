package events

import (
	"context"
	"database/sql"
	"log"
	"time"
)

// Tx is the slice of *sql.Tx (or *sql.Conn) an outbox write needs. It is the
// CALLER's transaction: Tether's database has one connection (MaxOpenConns=1),
// so a second connection from inside a transaction would deadlock.
type Tx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// EventIDPersister is a Persister that mints and persists a D6 event id when it
// inserts a row and reports it, so the bus can fan out the same id it stored.
type EventIDPersister interface {
	InsertEventIDContext(ctx context.Context, scope Scope, sessionID, kind, payloadJSON string) (seq int64, at time.Time, eventID string, err error)
}

// TxPersister adds the outbox: rows written on a caller's transaction are
// pending until the bus has published them after the commit.
type TxPersister interface {
	// InsertEventTx writes e on tx as a pending row, minting e.EventID unless the
	// caller supplied one. A repeat of an EventID already stored returns the
	// stored row and writes nothing (idempotent retry).
	InsertEventTx(ctx context.Context, tx Tx, e Event) (Event, error)
	// PendingEventsContext returns committed pending rows in ascending Seq; with
	// beforeSeq > 0 only those with Seq < beforeSeq.
	PendingEventsContext(ctx context.Context, beforeSeq int64, limit int) ([]Event, error)
	// MarkPublishedContext clears pending for the given sequences.
	MarkPublishedContext(ctx context.Context, seqs []int64) error
}

// OutboxBus is a Bus that can publish from a caller's transaction.
type OutboxBus interface {
	Bus
	// PublishTx writes e on tx and returns the stored event (Seq, At, EventID).
	// It takes no bus lock and uses no other connection. Nothing is fanned out
	// until the transaction commits and the outbox drains: call KickOutbox after
	// the commit (a rollback publishes nothing; a missed kick is caught by the
	// drainer's sweep). The returned event is not visible until the commit.
	PublishTx(ctx context.Context, tx Tx, e Event) (Event, error)
	// KickOutbox tells the drainer that committed pending rows may exist.
	KickOutbox()
	// StartOutbox runs the drainer until ctx is done or the returned stop is
	// called. It first publishes whatever a previous process committed but did
	// not publish.
	StartOutbox(ctx context.Context) (stop func())
}

const (
	outboxBatch      = 256
	defaultOutboxGap = 2 * time.Second
)

// PublishTx implements OutboxBus.
func (b *memBus) PublishTx(ctx context.Context, tx Tx, e Event) (Event, error) {
	if b.txp == nil {
		return Event{}, ErrNoTxPersister
	}
	if e.Scope == "" {
		return Event{}, ErrScopeEmpty
	}
	e = b.stamp(ctx, e, true)
	// Deliberately not under the bus lock: the caller holds the only database
	// connection inside its transaction, and a publisher holding the lock waits
	// for that connection.
	stored, err := b.txp.InsertEventTx(ctx, tx, e)
	if err != nil {
		return Event{}, err
	}
	b.dirty.Store(true)
	return stored, nil
}

// KickOutbox implements OutboxBus.
func (b *memBus) KickOutbox() {
	b.dirty.Store(true)
	select {
	case b.kick <- struct{}{}:
	default:
	}
}

// StartOutbox implements OutboxBus.
func (b *memBus) StartOutbox(ctx context.Context) (stop func()) {
	if b.txp == nil {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		b.dirty.Store(true) // leftovers of a previous process
		b.drainOnce(ctx)
		t := time.NewTicker(b.outboxGap)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.kick:
			case <-t.C:
				b.dirty.Store(true) // a missed kick, or a writer that never kicked
			}
			b.drainOnce(ctx)
		}
	}()
	return func() { cancel(); <-done }
}

// drainOnce publishes the committed pending rows under the bus lock.
func (b *memBus) drainOnce(ctx context.Context) {
	if err := b.lock(ctx); err != nil {
		return
	}
	defer b.unlock()
	if err := b.drainLocked(ctx, 0); err != nil && ctx.Err() == nil {
		log.Printf("events: outbox drain: %v", err)
	}
}

// drainLocked fans out the committed pending rows (those with Seq < beforeSeq
// when it is > 0, else all), oldest first, then marks them published. The bus
// lock is held, so an event the caller publishes next is delivered after them.
// A crash between fan-out and mark repeats the fan-out on restart (at least
// once). A full drain clears the dirty flag BEFORE it looks, so a PublishTx that
// lands after the look sets it again instead of being lost; on an error it stays
// set.
func (b *memBus) drainLocked(ctx context.Context, beforeSeq int64) error {
	if b.txp == nil {
		return nil
	}
	if beforeSeq <= 0 {
		b.dirty.Store(false)
	}
	for {
		rows, err := b.txp.PendingEventsContext(ctx, beforeSeq, outboxBatch)
		if err != nil {
			b.dirty.Store(true)
			return err
		}
		if len(rows) == 0 {
			return nil
		}
		seqs := make([]int64, 0, len(rows))
		for _, e := range rows {
			b.fanout(e)
			seqs = append(seqs, e.Seq)
		}
		if err := b.txp.MarkPublishedContext(ctx, seqs); err != nil {
			b.dirty.Store(true)
			return err
		}
		if len(rows) < outboxBatch {
			return nil
		}
	}
}
