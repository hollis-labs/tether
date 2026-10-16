package eventstamp

import (
	"context"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/callcontext"
	"github.com/hollis-labs/tether/internal/events"
)

// SessionRow is the part of a session row that becomes scope dimensions. These
// are Tether's own record of the session, so they are as trustworthy as the
// event's SessionID.
type SessionRow struct {
	WorkerID       string // blank means hub-local
	ProjectID      string
	WorkstreamID   string
	TaskID         string
	LaunchID       string
	LogicalAgentID string
}

// SessionSource reads session rows. It is called outside the bus lock, never
// from PublishTx.
type SessionSource interface {
	// Session reports false when there is no such session.
	Session(ctx context.Context, id string) (SessionRow, bool, error)
}

// Defaults for NewRows.
const (
	DefaultRowTTL      = 5 * time.Second
	DefaultMissTTL     = time.Second
	defaultRowCacheMax = 4096
)

type rowEntry struct {
	row     SessionRow
	found   bool
	expires time.Time
}

// Rows stamps session-row dimensions (notably worker_id) on events that name a
// session, for events published outside that session's own call context where
// the caller snapshot cannot supply them. It keeps a small read-through cache
// so a busy session costs one lookup per TTL, not one per event; a row's
// worker, project, launch and logical agent do not change after the launch, and
// workstream and task are bound at creation or by an explicit attach, so a few
// seconds of staleness is the bound.
//
// It is not TxSafe: a miss reads the database, and PublishTx runs inside a
// caller's transaction that holds the only connection.
type Rows struct {
	src     SessionSource
	ttl     time.Duration
	missTTL time.Duration
	max     int
	now     func() time.Time

	mu    sync.Mutex
	cache map[string]rowEntry
}

var _ events.Stage = (*Rows)(nil)

// NewRows returns the stage. ttl <= 0 uses DefaultRowTTL.
func NewRows(src SessionSource, ttl time.Duration) *Rows {
	if ttl <= 0 {
		ttl = DefaultRowTTL
	}
	return &Rows{src: src, ttl: ttl, missTTL: min(ttl, DefaultMissTTL), max: defaultRowCacheMax, now: time.Now, cache: map[string]rowEntry{}}
}

// Invalidate drops one session from the cache (after an explicit attach).
func (r *Rows) Invalidate(id string) {
	r.mu.Lock()
	delete(r.cache, id)
	r.mu.Unlock()
}

// Stamp fills empty dimensions from the event's own session row. A caller
// context carrying an unresolved snapshot means the session id may be only
// claimed, so nothing is stamped then; an event published by the daemon itself
// carries no snapshot and its SessionID is Tether's own.
func (r *Rows) Stamp(ctx context.Context, e *events.Event) error {
	if e.SessionID == "" || r.src == nil {
		return nil
	}
	if snap, ok := callcontext.FromContext(ctx); ok && !snap.SessionResolved {
		return nil
	}
	entry, err := r.lookup(ctx, e.SessionID)
	if err != nil || !entry.found {
		return err
	}
	d := &e.Envelope.Dims
	set(&d.WorkerID, entry.row.WorkerID)
	set(&d.ProjectID, entry.row.ProjectID)
	set(&d.WorkstreamID, entry.row.WorkstreamID)
	set(&d.TaskID, entry.row.TaskID)
	set(&d.LaunchID, entry.row.LaunchID)
	set(&d.LogicalAgentID, entry.row.LogicalAgentID)
	return nil
}

func (r *Rows) lookup(ctx context.Context, id string) (rowEntry, error) {
	now := r.now()
	r.mu.Lock()
	if hit, ok := r.cache[id]; ok && now.Before(hit.expires) {
		r.mu.Unlock()
		return hit, nil
	}
	r.mu.Unlock()
	row, found, err := r.src.Session(ctx, id)
	if err != nil {
		return rowEntry{}, err
	}
	entry := rowEntry{row: row, found: found, expires: now.Add(r.ttl)}
	if !found {
		entry.expires = now.Add(r.missTTL)
	}
	r.mu.Lock()
	if len(r.cache) >= r.max {
		clear(r.cache)
	}
	r.cache[id] = entry
	r.mu.Unlock()
	return entry, nil
}
