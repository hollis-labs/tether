package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/database"
)

// Errors the class discipline returns. Each wraps the detail with fmt.Errorf("%w: ...").
var (
	// ErrNestedWrite: a WriteTx or Joint started inside another one's callback. With one write
	// connection it would wait on the lock its own caller holds; after a class moves it would not be
	// atomic with its caller. Either way it is a bug.
	ErrNestedWrite = errors.New("store: write transaction inside another write transaction")
	// ErrCrossClassWrite: a statement wrote a table of a class the transaction does not hold.
	ErrCrossClassWrite = errors.New("store: write to a table of another class")
	// ErrUnknownJoint: Joint was called with a name that is not on the allowlist.
	ErrUnknownJoint = errors.New("store: joint transaction not on the allowlist")
	// ErrUnclassified: a statement wrote a table with no class, or could not be attributed to a table.
	ErrUnclassified = errors.New("store: statement not attributable to a classed table")
	// ErrLibraryUnavailable: the library-owned tables have no store on this dialect. go-messaging v0.10.0
	// has a SQLite store only (Substrate gap CW-20261115-0013).
	// ErrWriteOnReader: a write or an unattributable statement was sent to a Handle's read pool.
	ErrWriteOnReader = errors.New("store: write through the read pool")
	ErrLibraryUnavailable = errors.New("store: go-messaging has no Postgres store; its tables are SQLite only for now")
)

// Stores holds the three class handles. At the cutover they share one *store.Store (one write
// connection on the one SQLite database), so moving class B or C later is configuration, not code
// (plan section 13.1(4)).
type Stores struct {
	a, b, c *Handle
}

// New returns the class handles over one store.
func New(st *store.Store) *Stores {
	s := &Stores{}
	s.a = &Handle{class: A, st: st}
	s.b = &Handle{class: B, st: st}
	s.c = &Handle{class: C, st: st}
	return s
}

// FromApp returns the class handles over the database module's store. Call it after the module starts.
func FromApp(app *module.App) *Stores { return New(database.From(app)) }

// A returns the class A handle.
func (s *Stores) A() *Handle { return s.a }

// B returns the class B handle.
func (s *Stores) B() *Handle { return s.b }

// C returns the class C handle.
func (s *Stores) C() *Handle { return s.c }

// Handle reaches the tables of one class. Reads are not checked; every write goes through WriteTx, whose
// Tx refuses a statement that writes another class's table.
type Handle struct {
	class Class
	st    *store.Store
}

// Class returns the handle's class.
func (h *Handle) Class() Class { return h.class }

// Dialect returns the dialect of the class's store.
func (h *Handle) Dialect() store.Dialect { return h.st.Dialect() }

// Reader returns the read pool of the class's store. Placeholders are rebound for the dialect. It runs
// reads only: a write, or a statement it cannot attribute, is refused with ErrWriteOnReader (on Postgres
// the read pool is an ordinary pool, so a write there would bypass the class rule). Do not use it inside
// a WriteTx callback: read through the Tx there.
func (h *Handle) Reader() store.Querier { return readOnly{h.st.Reader()} }

// ReadTx runs fn in a read-only transaction on the class's store, for one consistent snapshot.
func (h *Handle) ReadTx(ctx context.Context, fn func(ctx context.Context, q store.Querier) error) error {
	return h.st.ReadTx(ctx, fn)
}

// WriteTx runs fn in a write transaction on the class's store. The Tx it passes refuses writes to any
// other class, and a WriteTx or Joint started from inside fn (through the ctx fn receives) is refused
// with ErrNestedWrite. fn may run more than once on contention, so it must be safe to retry.
func (h *Handle) WriteTx(ctx context.Context, fn func(ctx context.Context, tx Tx) error) error {
	return writeTx(ctx, h.st, "class "+h.class.String(), classSet(h.class), fn)
}

// Joint runs fn in one write transaction that may write the classes the allowlist grants name. It is
// the only way to write two classes atomically, and exists for the transactions listed in joints.go
// until their slices redesign them; each entry names that slice.
func (s *Stores) Joint(ctx context.Context, name string, fn func(ctx context.Context, tx Tx) error) error {
	j, ok := joints[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrUnknownJoint, name)
	}
	// One store today, so any handle's store serves. When a class moves to its own store, a joint over
	// it can no longer be atomic: Joint then refuses, and the redesign named in the allowlist is due.
	st := s.a.st
	for _, c := range j.classes {
		if s.handle(c).st != st {
			return fmt.Errorf("%w: %q spans classes on different stores; redesign due in %s", ErrUnknownJoint, name, j.slice)
		}
	}
	return writeTx(ctx, st, "joint "+name, classSet(j.classes...), fn)
}

// LibraryDB returns the database for library-owned tables (go-messaging's messaging_*, class C), which
// the library reads and writes through a raw *sql.DB. On SQLite it is the store's single writer, so the
// library's writes serialize with Tether's. It is the only sanctioned raw handle (TestNoRawDB).
//
// The rule: no library call runs inside a WriteTx or Joint callback. It would wait on the writer its own
// caller holds. Ask for the handle with the ctx in hand, just before the call; inside a callback that
// ctx is refused with ErrNestedWrite. On Postgres it returns ErrLibraryUnavailable.
func (s *Stores) LibraryDB(ctx context.Context) (*sql.DB, error) {
	if outer, ok := ctx.Value(txMarker{}).(string); ok {
		return nil, fmt.Errorf("%w: a library call inside %s", ErrNestedWrite, outer)
	}
	st := s.c.st
	if st.Dialect().Name() != store.DriverSQLite {
		return nil, ErrLibraryUnavailable
	}
	return st.DB(), nil
}

func (s *Stores) handle(c Class) *Handle {
	switch c {
	case A:
		return s.a
	case B:
		return s.b
	case C:
		return s.c
	}
	panic(fmt.Sprintf("store: no handle for %s", c))
}

type txMarker struct{}

func writeTx(ctx context.Context, st *store.Store, label string, allowed map[Class]bool, fn func(ctx context.Context, tx Tx) error) error {
	if outer, ok := ctx.Value(txMarker{}).(string); ok {
		return fmt.Errorf("%w: %s inside %s", ErrNestedWrite, label, outer)
	}
	inner := context.WithValue(ctx, txMarker{}, label)
	return st.WriteTx(inner, func(ctx context.Context, raw store.Tx) error {
		return fn(ctx, &checkedTx{Tx: raw, label: label, allowed: allowed})
	})
}

func classSet(cs ...Class) map[Class]bool {
	m := make(map[Class]bool, len(cs))
	for _, c := range cs {
		m[c] = true
	}
	return m
}
