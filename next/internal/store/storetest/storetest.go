// Package storetest gives store tests a migrated database on each dialect, with Tether's class handles
// over it, plus a fixed clock and deterministic ids.
//
// SQLite always runs. Postgres runs when SUBSTRATE_TEST_POSTGRES_DSN is set, each test on a private schema
// dropped at the end; without it the Postgres subtest skips with a notice (port plan section 13.1(3)).
package storetest

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/core/store/migrate"
	libtest "github.com/hollis-labs/libs/core/store/storetest"

	"github.com/hollis-labs/tether/next/internal/store"
	_ "github.com/hollis-labs/tether/next/internal/sqlitefk" // foreign keys off, as the binary runs
	"github.com/hollis-labs/tether/next/migrations"
)

// EnvPostgresDSN names the Postgres server for the Postgres leg.
const EnvPostgresDSN = "SUBSTRATE_TEST_POSTGRES_DSN"

// SkipNotice is the skip message of a Postgres test that did not run.
const SkipNotice = "NOTICE: " + EnvPostgresDSN + " is unset, so the Postgres leg did not run (port plan 13.1(3))"

// Open returns class handles over a fresh database of the dialect with Tether's migrations applied, or
// skips the test for Postgres without a server.
func Open(t *testing.T, d gostore.Driver) *store.Stores {
	t.Helper()
	return store.New(Raw(t, d))
}

// Raw is Open without the class handles, for tests that check the handles themselves.
func Raw(t *testing.T, d gostore.Driver) *gostore.Store {
	t.Helper()
	var st *gostore.Store
	switch d {
	case gostore.DriverSQLite:
		st = libtest.SQLite(t)
	case gostore.DriverPostgres:
		dsn := os.Getenv(EnvPostgresDSN)
		if dsn == "" {
			t.Skip(SkipNotice)
		}
		t.Setenv(libtest.EnvDSN, dsn)
		st = libtest.Postgres(t)
	default:
		t.Fatalf("storetest: unknown dialect %q", d)
	}
	fsys, root := migrations.FS()
	if err := migrate.UpStore(context.Background(), st, migrate.Source{FS: fsys, Root: root}); err != nil {
		t.Fatalf("storetest: migrate %s: %v", d, err)
	}
	return st
}

// Dialects runs fn as a subtest on each dialect, over class handles on a fresh migrated database.
func Dialects(t *testing.T, fn func(t *testing.T, s *store.Stores)) {
	t.Helper()
	for _, d := range []gostore.Driver{gostore.DriverSQLite, gostore.DriverPostgres} {
		t.Run(string(d), func(t *testing.T) { fn(t, Open(t, d)) })
	}
}

// Clock is a fixed clock that moves only when told.
type Clock struct {
	mu  sync.Mutex
	now time.Time
}

// NewClock starts a clock at a fixed instant (2026-11-14T00:00:00Z) unless at is given.
func NewClock(at ...time.Time) *Clock {
	c := &Clock{now: time.Date(2026, 11, 14, 0, 0, 0, 0, time.UTC)}
	if len(at) > 0 {
		c.now = at[0].UTC()
	}
	return c
}

// Now returns the clock's time.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// IDs hands out deterministic ids: prefix-1, prefix-2, ...
type IDs struct {
	mu     sync.Mutex
	prefix string
	n      int
}

// NewIDs returns an id generator with the prefix.
func NewIDs(prefix string) *IDs { return &IDs{prefix: prefix} }

// New returns the next id.
func (g *IDs) New() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n++
	return fmt.Sprintf("%s-%d", g.prefix, g.n)
}
