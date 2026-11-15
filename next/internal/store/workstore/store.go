// Package workstore is the class C store for workstreams and the environment view (port slice
// S2b-4, plan 13.1): workstreams and their projects, session-to-workstream assignment, the
// environment request and work-state bookkeeping, the environment event window and snapshot, and
// the raw reads behind the workstream digest. It reaches its tables through the class handles (C,
// and B for the events the environment view reads) and writes SQL that runs on SQLite and Postgres.
//
// The store speaks rows. What the old store (internal/store at main f6c37bfa) built on top is the
// workstream slice's: assembling a digest from sessions and refs (grouping, totals, coverage), and
// the attribution rules it applies. The environment request's own payload checks stay here, as the
// old store kept them in front of its bookkeeping rows.
package workstore

import (
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

	"github.com/hollis-labs/tether/next/internal/store"
	"github.com/hollis-labs/tether/next/internal/store/eventstore"
)

// Store is the workstream and environment store.
type Store struct {
	all *store.Stores
	b   *store.Handle
	c   *store.Handle
	// Events writes the bookkeeping kinds' events inside their joints, and pages the event window.
	Events *eventstore.Store
	// Now is the store's clock (time.Now outside tests).
	Now func() time.Time
}

// New returns the store over the class handles and the event store.
func New(s *store.Stores, events *eventstore.Store) *Store {
	return &Store{all: s, b: s.B(), c: s.C(), Events: events, Now: time.Now}
}

func (s *Store) now() time.Time { return s.Now().UTC() }

// secondStamp is the workstreams and sessions tables' time format: RFC 3339 to the second, as the
// old store wrote created_at and updated_at (migrated rows keep it).
func (s *Store) secondStamp() string { return s.now().Format(time.RFC3339) }

func rebind(q gostore.Querier, query string) string { return q.Dialect().Rebind(query) }

func isPostgres(q gostore.Querier) bool { return q.Dialect().Name() == gostore.DriverPostgres }

func nullText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }
