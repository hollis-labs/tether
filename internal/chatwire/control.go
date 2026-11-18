package chatwire

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	chat "github.com/hollis-labs/libs/agent/chat"
	"github.com/hollis-labs/libs/agent/sdk/chathost"
	"github.com/hollis-labs/libs/agent/sdk/session"
	"github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/runtimecontract"

	"github.com/hollis-labs/tether/internal/conversation"
)

// TerminalHolder is the holder a *chat.LeaseHeldError names when the chat host's writer lease is held by a client that
// is not a chat attachment (a /terminal-attachments writer). chathost has no read of the current lease, so its own
// attachment ID cannot be reported (see Gaps in doc.go).
const TerminalHolder = "terminal"

// ErrHostDrift is returned when the chat host's binding is ahead of, or disagrees with, the Tether conversations row at
// the same generation. The Tether row is the authority; this means the chat host state was written by something else.
var ErrHostDrift = errors.New("chatwire: chat host binding disagrees with the Tether conversation")

// ControlConfig configures a Control.
type ControlConfig struct {
	// DB holds the chat records chathost cannot: the conversation records the wire needs (active run, runner ref, reset
	// and successor, the chat lease) and the chat attachments. It is the chat database (Open), the same file as the
	// event log.
	DB *store.Store
	// Host is the chat host that terminal attachments share. Its writer lease is the one input lease.
	Host *chathost.Host
	// Seat returns the Tether conversation of an agent (a seat): conversation.ErrNotFound when it has none yet.
	Seat func(ctx context.Context, agent string) (string, error)
	// Target resolves a Tether conversation to where it runs now (conversation.Resolver.ByConversation). Its
	// generation is the authority for the chat conversation's.
	Target func(ctx context.Context, tetherConversationID string) (conversation.Target, error)
}

// Control is the chat.Control for Tether (CW-20261118-0011): the input lease lives in chathost, so chat attach and
// terminal attach share it, and the generation is the Tether conversations row's, bumped in the relaunch transaction.
// It does not implement chat.Bumper.
//
// Every method runs under one mutex: each is one atomic step within the daemon. chathost's own file lock covers other
// users of the same chat host root (terminal attachments).
type Control struct {
	cfg ControlConfig
	mu  sync.Mutex
}

var _ chat.Control = (*Control)(nil)

// NewControl returns a Control over cfg. The schema must already be applied (Open does that).
func NewControl(cfg ControlConfig) (*Control, error) {
	if cfg.DB == nil || cfg.Host == nil || cfg.Seat == nil || cfg.Target == nil {
		return nil, errors.New("chatwire: ControlConfig needs DB, Host, Seat and Target")
	}
	return &Control{cfg: cfg}, nil
}

const controlSchema = `
CREATE TABLE IF NOT EXISTS tether_chat_conversations (
	id                  TEXT PRIMARY KEY,
	agent               TEXT    NOT NULL,
	scope               TEXT    NOT NULL,
	owner               TEXT    NOT NULL,
	kind                TEXT    NOT NULL,
	tether_conversation TEXT    NOT NULL,
	generation          INTEGER NOT NULL,
	runner_ref          TEXT    NOT NULL DEFAULT '',
	active_run          TEXT    NOT NULL DEFAULT '',
	lease_holder        TEXT    NOT NULL DEFAULT '',
	lease_expires_ns    INTEGER NOT NULL DEFAULT 0,
	host_lease          TEXT    NOT NULL DEFAULT '',
	closed              INTEGER NOT NULL DEFAULT 0,
	successor           TEXT    NOT NULL DEFAULT '',
	created_ns          INTEGER NOT NULL
) WITHOUT ROWID;
CREATE UNIQUE INDEX IF NOT EXISTS tether_chat_conversations_current ON tether_chat_conversations (agent) WHERE closed = 0;
CREATE INDEX IF NOT EXISTS tether_chat_conversations_tether ON tether_chat_conversations (tether_conversation);
CREATE TABLE IF NOT EXISTS tether_chat_attachments (
	conversation_id TEXT    NOT NULL,
	id              TEXT    NOT NULL,
	role            TEXT    NOT NULL,
	stream          TEXT    NOT NULL,
	generation      INTEGER NOT NULL,
	created_ns      INTEGER NOT NULL,
	host_attachment TEXT    NOT NULL,
	PRIMARY KEY (conversation_id, id)
) WITHOUT ROWID;
`

// applySchema creates the Control's tables in db when missing.
func applySchema(ctx context.Context, db *store.Store) error {
	return db.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, controlSchema)
		return err
	})
}

// record is a chat conversation as stored: the wire's record plus what ties it to Tether and to chathost.
type record struct {
	chat.Conversation
	tether    string         // the Tether conversation it runs in
	hostLease *session.Lease // the chathost writer lease taken for the chat lease holder
	target    conversation.Target
}

type attRecord struct {
	chat.Attachment
	host chathost.Attachment
}

const convColumns = `id, agent, scope, owner, kind, tether_conversation, generation, runner_ref, active_run,
	lease_holder, lease_expires_ns, host_lease, closed, successor, created_ns`

func scanRecord(row *sql.Row) (*record, error) {
	var (
		r               record
		holder, hl      string
		expires, create int64
	)
	err := row.Scan(&r.ID, &r.Agent, &r.Scope, &r.Owner, &r.Kind, &r.tether, &r.Generation, &r.RunnerRef, &r.ActiveRun,
		&holder, &expires, &hl, &r.Closed, &r.Successor, &create)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, chat.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	r.CreatedAt = time.Unix(0, create).UTC()
	if holder != "" {
		r.Lease = &chat.Lease{Holder: holder, ExpiresAt: time.Unix(0, expires).UTC()}
	}
	if hl != "" {
		var l session.Lease
		if err := json.Unmarshal([]byte(hl), &l); err != nil {
			return nil, fmt.Errorf("decode host lease: %w", err)
		}
		r.hostLease = &l
	}
	return &r, nil
}

func (c *Control) load(ctx context.Context, where string, arg any) (*record, error) {
	return scanRecord(c.cfg.DB.Reader().QueryRowContext(ctx, `SELECT `+convColumns+` FROM tether_chat_conversations WHERE `+where, arg)) //nolint:gosec // where is a constant clause
}

func insertRecord(ctx context.Context, tx store.Tx, r *record) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO tether_chat_conversations(id, agent, scope, owner, kind, tether_conversation, generation, created_ns)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, r.ID, r.Agent, r.Scope, r.Owner, r.Kind, r.tether, r.Generation, r.CreatedAt.UnixNano())
	if err != nil {
		return err
	}
	return saveRecord(ctx, tx, r)
}

func saveRecord(ctx context.Context, tx store.Tx, r *record) error {
	var (
		holder, hl string
		expires    int64
	)
	if r.Lease != nil {
		holder, expires = r.Lease.Holder, r.Lease.ExpiresAt.UnixNano()
	}
	if r.hostLease != nil {
		b, err := json.Marshal(r.hostLease)
		if err != nil {
			return err
		}
		hl = string(b)
	}
	_, err := tx.ExecContext(ctx, `UPDATE tether_chat_conversations SET generation = ?, runner_ref = ?, active_run = ?,
		lease_holder = ?, lease_expires_ns = ?, host_lease = ?, closed = ?, successor = ? WHERE id = ?`,
		r.Generation, r.RunnerRef, r.ActiveRun, holder, expires, hl, r.Closed, r.Successor, r.ID)
	return err
}

func (c *Control) save(ctx context.Context, r *record) error {
	return c.cfg.DB.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error { return saveRecord(ctx, tx, r) })
}

func (c *Control) loadAtt(ctx context.Context, conversationID, id string) (*attRecord, error) {
	if id == "" {
		return nil, nil
	}
	a := attRecord{Attachment: chat.Attachment{ID: id, ConversationID: conversationID}}
	var (
		created int64
		host    string
	)
	err := c.cfg.DB.Reader().QueryRowContext(ctx,
		`SELECT role, stream, generation, created_ns, host_attachment FROM tether_chat_attachments WHERE conversation_id = ? AND id = ?`,
		conversationID, id).Scan(&a.Role, &a.Stream, &a.Generation, &created, &host)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a.CreatedAt = time.Unix(0, created).UTC()
	if err := json.Unmarshal([]byte(host), &a.host); err != nil {
		return nil, fmt.Errorf("decode host attachment: %w", err)
	}
	return &a, nil
}

// reconcile brings an open record in line with its Tether conversation, which is the generation authority: a higher
// Tether generation makes every attachment stale and clears the lease and the active run (the bump the relaunch
// transaction already made). It then moves the chat host binding to the same generation and attempt, so chathost,
// the conversations row and the chat record carry one number.
func (c *Control) reconcile(ctx context.Context, r *record) error {
	if r.Closed {
		return nil
	}
	t, err := c.cfg.Target(ctx, r.tether)
	if err != nil {
		return fmt.Errorf("resolve Tether conversation %s: %w", r.tether, err)
	}
	r.target = t
	switch {
	case t.Generation > r.Generation:
		r.Generation, r.Lease, r.ActiveRun, r.hostLease = t.Generation, nil, "", nil
		if err := c.save(ctx, r); err != nil {
			return err
		}
	case t.Generation < r.Generation:
		return fmt.Errorf("%w: chat record at generation %d, Tether at %d", ErrHostDrift, r.Generation, t.Generation)
	}
	return syncHost(ctx, c.cfg.Host, r.binding())
}

// binding is the chat host binding of a record: one chathost conversation per Tether conversation, whatever chat
// conversation (before or after a reset) currently runs in it.
func (r *record) binding() runtimecontract.Binding {
	return HostBinding(r.Owner, r.Agent, r.Scope, r.target)
}

// HostBinding is the chat host binding for a conversation's agent and its current Tether target. Terminal attachments
// (CW-20261118-0012) must derive their binding the same way to share the input lease.
func HostBinding(owner, agent, scope string, t conversation.Target) runtimecontract.Binding {
	return runtimecontract.Binding{
		OwnerID:        runtimecontract.OwnerID(owner),
		OperativeID:    runtimecontract.OperativeID(agent),
		SeatID:         runtimecontract.SeatID(scope),
		ConversationID: runtimecontract.ConversationID(t.ConversationID),
		AttemptID:      runtimecontract.AttemptID(t.AttemptID),
		Generation:     runtimecontract.Generation(t.Generation),
	}
}

// change loads an open-or-closed record and the named attachment, reconciles the record, applies f and saves the
// record when f succeeds. The record is reconciled (and that saved) even when f fails.
func (c *Control) change(ctx context.Context, conversationID, attachmentID string, f func(r *record, a *attRecord) error) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load(ctx, `id = ?`, conversationID)
	if err != nil {
		return err
	}
	if err := c.reconcile(ctx, r); err != nil {
		return err
	}
	a, err := c.loadAtt(ctx, conversationID, attachmentID)
	if err != nil {
		return err
	}
	before := fingerprint(r)
	ferr := f(r, a)
	if fingerprint(r) == before {
		return ferr
	}
	if serr := c.save(ctx, r); serr != nil {
		return errors.Join(ferr, serr)
	}
	return ferr
}

// fingerprint is everything saveRecord writes, to skip a write when nothing changed.
func fingerprint(r *record) string {
	b, _ := json.Marshal([]any{r.Generation, r.RunnerRef, r.ActiveRun, r.Lease, r.hostLease, r.Closed, r.Successor}) // plain values: cannot fail
	return string(b)
}

// ---- the lease rules (the same rules as libs/agent/chat's built-in Controls) ----

func open(r *record) error {
	if r.Closed {
		return &chat.ClosedError{ConversationID: r.ID, Successor: r.Successor}
	}
	return nil
}

func current(r *record, a *attRecord) error {
	if err := open(r); err != nil {
		return err
	}
	if a == nil {
		return chat.ErrUnknownAttachment
	}
	if a.Generation != r.Generation {
		return &chat.StaleError{Generation: r.Generation}
	}
	return nil
}

// hostTake gives a's chat host attachment the writer lease. force is a takeover. A chat host lease taken for an
// earlier chat holder, which the chat side has let lapse, is revoked first while it is still that holder's.
func (c *Control) hostTake(ctx context.Context, r *record, a *attRecord, force bool) error {
	b := r.binding()
	writer := session.WriterID(a.host.ID)
	var (
		l   session.Lease
		err error
	)
	switch {
	case force:
		l, err = c.cfg.Host.Takeover(ctx, b, writer)
	case r.hostLease != nil && r.hostLease.Writer == writer:
		if l, err = c.cfg.Host.Renew(ctx, *r.hostLease); err != nil {
			l, err = c.cfg.Host.Acquire(ctx, b, writer)
		}
	default:
		if r.hostLease != nil && c.cfg.Host.ValidateLease(ctx, *r.hostLease) == nil {
			if rerr := c.cfg.Host.Revoke(ctx, b); rerr != nil {
				return rerr
			}
		}
		l, err = c.cfg.Host.Acquire(ctx, b, writer)
	}
	if errors.Is(err, session.ErrLease) {
		return &chat.LeaseHeldError{Lease: chat.Lease{Holder: TerminalHolder}}
	}
	if err != nil {
		return err
	}
	r.hostLease = &l
	return nil
}

// hostHolds checks that the chat lease holder a still has the chat host's writer lease. chathost leases last 30
// seconds on its own clock; one that lapsed while nobody else took it is taken again. When another writer (a terminal)
// holds it, the chat lease is lost.
func (c *Control) hostHolds(ctx context.Context, r *record, a *attRecord) error {
	if r.hostLease != nil && r.hostLease.Writer == session.WriterID(a.host.ID) && c.cfg.Host.ValidateLease(ctx, *r.hostLease) == nil {
		return nil
	}
	l, err := c.cfg.Host.Acquire(ctx, r.binding(), session.WriterID(a.host.ID))
	if err != nil {
		r.Lease, r.hostLease = nil, nil
		if errors.Is(err, session.ErrLease) {
			return chat.ErrLeaseRequired
		}
		return err
	}
	r.hostLease = &l
	return nil
}

// ---- chat.Control ----

// Ensure implements chat.Control. The agent's first chat conversation takes the Tether conversation's ID; one opened
// by a reset takes nc.ID. Either starts at the Tether conversation's current generation.
func (c *Control) Ensure(ctx context.Context, nc chat.NewConversation) (chat.Conversation, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load(ctx, `agent = ? AND closed = 0`, nc.Agent)
	if err == nil {
		if err := c.reconcile(ctx, r); err != nil {
			return chat.Conversation{}, false, err
		}
		return r.Conversation, false, nil
	}
	if !errors.Is(err, chat.ErrNotFound) {
		return chat.Conversation{}, false, err
	}
	tether, err := c.cfg.Seat(ctx, nc.Agent)
	if errors.Is(err, conversation.ErrNotFound) {
		return chat.Conversation{}, false, fmt.Errorf("agent %q has no Tether conversation yet: %w", nc.Agent, chat.ErrNotFound)
	}
	if err != nil {
		return chat.Conversation{}, false, err
	}
	id := tether
	if _, lerr := c.load(ctx, `id = ?`, tether); lerr == nil {
		id = nc.ID // the Tether ID already names an earlier (reset) chat conversation
	}
	r = &record{
		Conversation: chat.Conversation{ID: id, Agent: nc.Agent, Scope: nc.Scope, Owner: nc.Owner, Kind: nc.Kind, CreatedAt: nc.At},
		tether:       tether,
	}
	if r.target, err = c.cfg.Target(ctx, tether); err != nil {
		return chat.Conversation{}, false, err
	}
	r.Generation = r.target.Generation
	if err := syncHost(ctx, c.cfg.Host, r.binding()); err != nil {
		return chat.Conversation{}, false, err
	}
	if err := c.cfg.DB.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error { return insertRecord(ctx, tx, r) }); err != nil {
		return chat.Conversation{}, false, err
	}
	return r.Conversation, true, nil
}

// Get implements chat.Control. It reconciles an open conversation with its Tether row first, so the Service sees a
// generation the relaunch transaction raised.
func (c *Control) Get(ctx context.Context, conversationID string) (chat.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load(ctx, `id = ?`, conversationID)
	if err != nil {
		return chat.Conversation{}, err
	}
	if err := c.reconcile(ctx, r); err != nil {
		return chat.Conversation{}, err
	}
	return r.Conversation, nil
}

// Reset implements chat.Control. The new conversation runs in the same Tether conversation (and chat host
// conversation); the old one's chat host attachments are detached.
func (c *Control) Reset(ctx context.Context, conversationID, attachmentID string, next chat.NewConversation, now time.Time) (chat.Conversation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.load(ctx, `id = ?`, conversationID)
	if err != nil {
		return chat.Conversation{}, err
	}
	if err := c.reconcile(ctx, r); err != nil {
		return chat.Conversation{}, err
	}
	if err := open(r); err != nil {
		return chat.Conversation{}, err
	}
	var a *attRecord
	if attachmentID != "" {
		if a, err = c.loadAtt(ctx, conversationID, attachmentID); err != nil {
			return chat.Conversation{}, err
		}
		if err := current(r, a); err != nil {
			return chat.Conversation{}, err
		}
	}
	if held, ok := r.LeaseAt(now); ok && (a == nil || held.Holder != a.ID) {
		return chat.Conversation{}, chat.ErrLeaseRequired
	}
	if r.ActiveRun != "" {
		return chat.Conversation{}, chat.ErrBusy
	}
	if err := c.detachHost(ctx, conversationID); err != nil {
		return chat.Conversation{}, err
	}
	r.Closed, r.Successor, r.Lease, r.hostLease = true, next.ID, nil, nil
	fresh := &record{
		Conversation: chat.Conversation{ID: next.ID, Agent: r.Agent, Scope: next.Scope, Owner: next.Owner, Kind: next.Kind,
			Generation: r.Generation, CreatedAt: next.At},
		tether: r.tether, target: r.target,
	}
	err = c.cfg.DB.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := saveRecord(ctx, tx, r); err != nil {
			return err
		}
		return insertRecord(ctx, tx, fresh)
	})
	if err != nil {
		return chat.Conversation{}, err
	}
	return fresh.Conversation, nil
}

// detachHost removes every chat host attachment of a chat conversation (and with them any writer lease they hold).
func (c *Control) detachHost(ctx context.Context, conversationID string) error {
	rows, err := c.cfg.DB.Reader().QueryContext(ctx, `SELECT host_attachment FROM tether_chat_attachments WHERE conversation_id = ?`, conversationID)
	if err != nil {
		return err
	}
	var atts []chathost.Attachment
	for rows.Next() {
		var raw string
		var a chathost.Attachment
		if err := rows.Scan(&raw); err == nil && json.Unmarshal([]byte(raw), &a) == nil {
			atts = append(atts, a)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, a := range atts {
		// An attachment from an older generation is already gone from the chat host.
		if err := c.cfg.Host.Detach(ctx, a); err != nil && !errors.Is(err, session.ErrStale) && !errors.Is(err, session.ErrNotFound) {
			return err
		}
	}
	return nil
}

// SetRunnerRef implements chat.Control.
func (c *Control) SetRunnerRef(ctx context.Context, conversationID, ref string) error {
	return c.change(ctx, conversationID, "", func(r *record, _ *attRecord) error {
		if err := open(r); err != nil {
			return err
		}
		r.RunnerRef = ref
		return nil
	})
}

// Attach implements chat.Control. The attachment gets a chat host attachment of the same role on the current binding.
func (c *Control) Attach(ctx context.Context, a chat.Attachment) (chat.Attachment, error) {
	err := c.change(ctx, a.ConversationID, "", func(r *record, _ *attRecord) error {
		if err := open(r); err != nil {
			return err
		}
		if a.ID == "" {
			return errors.New("attachment id is empty")
		}
		a.Generation = r.Generation
		host, err := c.cfg.Host.Attach(ctx, r.binding(), chathost.Role(a.Role))
		if err != nil {
			return fmt.Errorf("chat host attach: %w", err)
		}
		raw, err := json.Marshal(host)
		if err != nil {
			return err
		}
		return c.cfg.DB.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO tether_chat_attachments(conversation_id, id, role, stream, generation, created_ns, host_attachment)
				VALUES(?, ?, ?, ?, ?, ?, ?)`, a.ConversationID, a.ID, string(a.Role), string(a.Stream), a.Generation, a.CreatedAt.UnixNano(), string(raw))
			return err
		})
	})
	return a, err
}

// Attachment implements chat.Control.
func (c *Control) Attachment(ctx context.Context, conversationID, attachmentID string) (chat.Attachment, error) {
	var out chat.Attachment
	err := c.change(ctx, conversationID, attachmentID, func(r *record, a *attRecord) error {
		if err := current(r, a); err != nil {
			return err
		}
		out = a.Attachment
		return nil
	})
	return out, err
}

// Detach implements chat.Control.
func (c *Control) Detach(ctx context.Context, conversationID, attachmentID string, now time.Time) (bool, error) {
	var released bool
	err := c.change(ctx, conversationID, attachmentID, func(r *record, a *attRecord) error {
		if err := current(r, a); err != nil {
			return err
		}
		if err := c.cfg.Host.Detach(ctx, a.host); err != nil && !errors.Is(err, session.ErrStale) && !errors.Is(err, session.ErrNotFound) {
			return fmt.Errorf("chat host detach: %w", err)
		}
		if r.hostLease != nil && r.hostLease.Writer == session.WriterID(a.host.ID) {
			r.hostLease = nil
		}
		if r.Lease != nil && r.Lease.Holder == a.ID {
			_, released = r.LeaseAt(now)
			r.Lease = nil
		}
		return c.cfg.DB.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
			_, err := tx.ExecContext(ctx, `DELETE FROM tether_chat_attachments WHERE conversation_id = ? AND id = ?`, conversationID, attachmentID)
			return err
		})
	})
	return released, err
}

func (c *Control) take(ctx context.Context, conversationID, attachmentID string, now time.Time, ttl time.Duration, force bool) (chat.LeaseChange, error) {
	var out chat.LeaseChange
	err := c.change(ctx, conversationID, attachmentID, func(r *record, a *attRecord) error {
		if err := current(r, a); err != nil {
			return err
		}
		if a.Role != chat.RoleController {
			return chat.ErrNotController
		}
		held, ok := r.LeaseAt(now)
		next := chat.Lease{Holder: a.ID, ExpiresAt: now.Add(ttl)}
		switch {
		case ok && held.Holder == a.ID:
			if err := c.hostTake(ctx, r, a, false); err != nil {
				return err
			}
			r.Lease, out = &next, chat.LeaseChange{Lease: next}
			return nil
		case ok && !force:
			return &chat.LeaseHeldError{Lease: held}
		}
		if err := c.hostTake(ctx, r, a, force); err != nil {
			return err
		}
		r.Lease, out = &next, chat.LeaseChange{Lease: next, Acquired: true, Previous: held.Holder}
		return nil
	})
	return out, err
}

// Acquire implements chat.Control.
func (c *Control) Acquire(ctx context.Context, conversationID, attachmentID string, now time.Time, ttl time.Duration) (chat.LeaseChange, error) {
	return c.take(ctx, conversationID, attachmentID, now, ttl, false)
}

// Takeover implements chat.Control. It replaces the chat host writer lease too, whoever holds it (a terminal
// included).
func (c *Control) Takeover(ctx context.Context, conversationID, attachmentID string, now time.Time, ttl time.Duration) (chat.LeaseChange, error) {
	return c.take(ctx, conversationID, attachmentID, now, ttl, true)
}

// Holds implements chat.Control.
func (c *Control) Holds(ctx context.Context, conversationID, attachmentID string, now time.Time) error {
	return c.change(ctx, conversationID, attachmentID, func(r *record, a *attRecord) error {
		if err := current(r, a); err != nil {
			return err
		}
		if held, ok := r.LeaseAt(now); !ok || held.Holder != a.ID {
			return chat.ErrLeaseRequired
		}
		return c.hostHolds(ctx, r, a)
	})
}

// BeginRun implements chat.Control.
func (c *Control) BeginRun(ctx context.Context, conversationID, runID, attachmentID string, now time.Time, ttl time.Duration) (chat.LeaseChange, error) {
	var out chat.LeaseChange
	err := c.change(ctx, conversationID, attachmentID, func(r *record, a *attRecord) error {
		if attachmentID == "" {
			if err := open(r); err != nil {
				return err
			}
			if r.ActiveRun != "" {
				return chat.ErrBusy
			}
			r.ActiveRun = runID
			return nil
		}
		if err := current(r, a); err != nil {
			return err
		}
		held, ok := r.LeaseAt(now)
		var change chat.LeaseChange
		switch {
		case ok && held.Holder == a.ID:
			if err := c.hostHolds(ctx, r, a); err != nil {
				return err
			}
			change.Lease = held
		case ok || a.Role != chat.RoleController:
			return chat.ErrLeaseRequired
		default:
			change = chat.LeaseChange{Lease: chat.Lease{Holder: a.ID, ExpiresAt: now.Add(ttl)}, Acquired: true}
		}
		if r.ActiveRun != "" {
			return chat.ErrBusy // checked before taking the lease: a refused submit acquires nothing
		}
		if change.Acquired {
			if err := c.hostTake(ctx, r, a, false); err != nil {
				if errors.Is(err, chat.ErrLeaseHeld) {
					return chat.ErrLeaseRequired // a terminal writer holds the one input lease
				}
				return err
			}
			l := change.Lease
			r.Lease = &l
		}
		r.ActiveRun, out = runID, change
		return nil
	})
	return out, err
}

// EndRun implements chat.Control.
func (c *Control) EndRun(ctx context.Context, conversationID, runID string) error {
	return c.change(ctx, conversationID, "", func(r *record, _ *attRecord) error {
		if r.ActiveRun == runID {
			r.ActiveRun = ""
		}
		return nil
	})
}

// TetherConversation returns the Tether conversation a chat conversation runs in. It reads only the chat database.
func (c *Control) TetherConversation(ctx context.Context, conversationID string) (string, error) {
	var id string
	err := c.cfg.DB.Reader().QueryRowContext(ctx, `SELECT tether_conversation FROM tether_chat_conversations WHERE id = ?`, conversationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", chat.ErrNotFound
	}
	return id, err
}

// CurrentFor returns the open chat conversation that runs in a Tether conversation, or chat.ErrNotFound when no chat
// client has opened one.
func (c *Control) CurrentFor(ctx context.Context, tetherConversationID string) (string, error) {
	var id string
	err := c.cfg.DB.Reader().QueryRowContext(ctx,
		`SELECT id FROM tether_chat_conversations WHERE tether_conversation = ? AND closed = 0`, tetherConversationID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", chat.ErrNotFound
	}
	return id, err
}

// Sync reconciles the open chat conversation of a Tether conversation now (the relaunch path calls it after the
// transaction that bumped the generation commits) and returns its ID, or chat.ErrNotFound when it has none.
func (c *Control) Sync(ctx context.Context, tetherConversationID string) (string, error) {
	id, err := c.CurrentFor(ctx, tetherConversationID)
	if err != nil {
		return "", err
	}
	_, err = c.Get(ctx, id)
	return id, err
}
