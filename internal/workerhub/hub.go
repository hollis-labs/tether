// Package workerhub owns the hub's live outpost channels, never peer authorities.
package workerhub

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// Worker placement failures callers can classify with errors.Is.
var (
	ErrNotEnrolled        = errors.New("not enrolled")
	ErrOffline            = errors.New("offline")
	ErrChannelUnavailable = errors.New("worker channel unavailable")
)

type Record struct {
	ID        string          `json:"id"`
	Label     string          `json:"label"`
	DeviceID  string          `json:"device_id"`
	Platform  string          `json:"platform"`
	Caps      json.RawMessage `json:"caps"`
	LastSeen  string          `json:"last_seen"`
	Connected bool            `json:"connected"`
	Sessions  int             `json:"sessions"`
	// Hosts is the worker's last-reported shim-host inventory (empty when offline or an older worker).
	Hosts []workerwire.Host `json:"hosts,omitempty"`
	// Orphaned counts hosts flagged orphaned in Hosts.
	Orphaned int `json:"orphaned_hosts"`
	// Delivery health of a connected worker (CW-20261114-0104): the run of
	// commands that got no answer in time, the session frames waiting to be
	// ingested, and the frames dropped on a full queue (replayed later).
	DeliveryTimeouts int   `json:"delivery_timeouts"`
	IngestBacklog    int   `json:"ingest_backlog"`
	DroppedFrames    int64 `json:"dropped_frames"`
}
type connection struct {
	activity         *workerwire.Activity
	activityReceived time.Time
	features         []string
	ws               *websocket.Conn
	write            sync.Mutex
	pendingMu        sync.Mutex
	pending          map[string]chan workerwire.Frame
	inventory        map[string]workerwire.Session
	hosts            []workerwire.Host
	replaying        map[string]bool
	// restore is the hello's start-up restore counts; nil from a worker that
	// predates them (CW-20261114-0056), whose inventory is not known complete.
	restore *workerwire.RestoreCounts
	done    chan struct{}

	// The reader only routes responses and queues the rest (CW-20261114-0104):
	// session frames go to ingest (Event, one goroutine, in order) and worker
	// requests to control. Neither ever blocks the reader. A session whose frame
	// did not fit is in dropped (under pendingMu) until ingest drains and asks
	// the worker to replay it from the stored cursor.
	ingest  chan workerwire.Frame
	control chan workerwire.Frame
	dropped map[string]bool
	// controlDone is closed when the control goroutine has finished: the
	// connection's mail bindings are dropped only after any bind it was
	// answering has completed, as when binds ran on the reader.
	controlDone chan struct{}
	// ingestDone is closed when the ingest goroutine has finished (or was never
	// started). A replacement connection's ingest waits for it, so an Event
	// still running for the old connection never races the new one's frames of
	// the same session.
	ingestDone chan struct{}
	// droppedFrames counts frames dropped on a full queue; timeouts is the run of
	// Sends that ran out of time with no answer; lastFrame (unix nanos) is when
	// the reader last received anything.
	droppedFrames atomic.Int64
	timeouts      atomic.Int32
	lastFrame     atomic.Int64
}

// ingestHandoverWait bounds how long a replacement connection's ingest waits for the old one's to finish.
// The app's ingest lock still serializes Events past it; the wait keeps the old connection's last frames
// ahead of the new connection's.
var ingestHandoverWait = 30 * time.Second

// Queue sizes for one worker connection. ingestQueue absorbs a burst of output
// from every session on the worker; past it, frames are dropped and replayed.
const (
	ingestQueue  = 4096
	controlQueue = 64
)

// DegradedAfter consecutive Send timeouts to a connected worker report it
// degraded (Hub.Degraded, action "reported"); ReconnectAfter close its
// connection so it reconnects and resumes (action "reconnect"): the recovery a
// hub restart gave by hand in CW-20261114-0104.
const (
	DegradedAfter  = 3
	ReconnectAfter = 6
)

// DeliveryDegraded is what Hub.Degraded reports about a worker that stopped
// answering commands.
type DeliveryDegraded struct {
	ConsecutiveTimeouts int    `json:"consecutive_timeouts"`
	IngestBacklog       int    `json:"ingest_backlog"`
	DroppedFrames       int64  `json:"dropped_frames"`
	Action              string `json:"action"`
}

func (c *connection) send(f workerwire.Frame) error {
	c.write.Lock()
	defer c.write.Unlock()
	_ = c.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return c.ws.WriteJSON(f)
}

type Hub struct {
	mailMu      sync.Mutex
	mail        map[string]*mailBinding
	BindMail    func(context.Context, string, workerwire.Binding) (workerwire.Binding, error)
	UnbindMail  func(context.Context, string, workerwire.Binding) error
	db          *sql.DB
	mu          sync.RWMutex
	connections map[string]*connection
	// lastIngest is each worker's most recent connection's ingestDone (under mu): a new connection's
	// ingest waits for it whether the old connection was replaced or had already gone.
	lastIngest map[string]<-chan struct{}
	// Event is installed by the app for worker-bound output/state ingestion. It
	// runs on the connection's ingest goroutine, never on its reader.
	Event func(string, workerwire.Frame) error
	// Degraded, when set, is told when Sends to a connected worker keep timing
	// out (CW-20261114-0104).
	Degraded func(workerID string, d DeliveryDegraded)
	// KillOrdered, when set, is told of every session a welcome orders the
	// worker to kill, with why (CW-20261114-0168), before the welcome is sent.
	KillOrdered func(workerID, sessionID, reason string)
	// lookupHeld answers what the hub knows of a session a reconnecting worker
	// holds; nil is the database query (heldSession). Tests replace it.
	lookupHeld func(ctx context.Context, sessionID, workerID string) (state string, seq int64, err error)
}

// welcomeLookupTimeout bounds the handshake's lookups of the sessions a worker
// holds. They run on a context detached from the connection's: a stream that is
// canceled mid-handshake must not turn every remaining lookup into an error.
var welcomeLookupTimeout = 10 * time.Second

// heldSession is the hub's record of a session a worker holds: its state and
// stored cursor, or sql.ErrNoRows when this hub has no such session on this worker.
func (h *Hub) heldSession(ctx context.Context, sessionID, workerID string) (string, int64, error) {
	if h.lookupHeld != nil {
		return h.lookupHeld(ctx, sessionID, workerID)
	}
	var state string
	var seq int64
	err := h.db.QueryRowContext(ctx, `SELECT s.state,COALESCE(q.last_seq,0) FROM sessions s LEFT JOIN session_worker_seq q ON q.session_id=s.id WHERE s.id=? AND s.worker_id=?`, sessionID, workerID).Scan(&state, &seq)
	return state, seq, err
}

// welcomeVerdict is what a welcome does with one session the worker holds. A
// kill needs a definite answer (CW-20261114-0168): the session is terminal here,
// or this hub has no such session on this worker. A lookup that failed (a
// stalled database, a timeout) is no answer: the session is held, left
// running and replayed once the hub can read it, never killed.
func welcomeVerdict(state string, err error) (kill bool, reason string, held bool) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return true, "unknown_to_hub", false
	case err != nil:
		return false, "", true
	case state == "completed" || state == "failed" || state == "killed":
		return true, "hub_state_" + state, false
	}
	return false, "", false
}

func New(db *sql.DB) *Hub {
	return &Hub{db: db, connections: make(map[string]*connection), lastIngest: map[string]<-chan struct{}{}, mail: map[string]*mailBinding{}}
}
func (h *Hub) Close() {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.connections {
		_ = c.ws.Close()
	}
}
func (h *Hub) Connected(id string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.connections[id] != nil
}

// Supports reports whether the connected worker advertised feature in its hello.
func (h *Hub) Supports(id, name string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	c := h.connections[id]
	return c != nil && feature(c.features, name)
}

// DeviceID returns the paired-device principal id behind a worker.
func (h *Hub) DeviceID(ctx context.Context, workerID string) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT device_id FROM workers WHERE id=?`, workerID).Scan(&id)
	return id, err
}
func (h *Hub) Resolve(ctx context.Context, name string) (string, error) {
	var id string
	err := h.db.QueryRowContext(ctx, `SELECT id FROM workers WHERE id=? OR (label=? AND retired_at IS NULL) ORDER BY (id=?) DESC, last_seen DESC LIMIT 1`, name, name, name).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("worker %q: %w", name, ErrNotEnrolled)
	}
	return id, nil
}
func (h *Hub) List(ctx context.Context) ([]Record, error) {
	rows, err := h.db.QueryContext(ctx, `SELECT w.id,w.label,w.device_id,w.platform,w.caps_json,w.last_seen,(SELECT COUNT(*) FROM sessions s WHERE s.worker_id=w.id AND s.state IN ('launching','running','idle')) FROM workers w WHERE w.retired_at IS NULL ORDER BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Record{}
	for rows.Next() {
		var r Record
		var caps string
		if err := rows.Scan(&r.ID, &r.Label, &r.DeviceID, &r.Platform, &caps, &r.LastSeen, &r.Sessions); err != nil {
			return nil, err
		}
		r.Caps = json.RawMessage(caps)
		r.Connected = h.Connected(r.ID)
		r.Hosts = h.hosts(r.ID)
		h.mu.RLock()
		if c := h.connections[r.ID]; c != nil {
			r.DeliveryTimeouts, r.IngestBacklog, r.DroppedFrames = int(c.timeouts.Load()), len(c.ingest), c.droppedFrames.Load()
		}
		h.mu.RUnlock()
		for _, host := range r.Hosts {
			if host.Orphaned {
				r.Orphaned++
			}
		}
		result = append(result, r)
	}
	return result, rows.Err()
}
func (h *Hub) ListHTTP(w http.ResponseWriter, r *http.Request) {
	records, err := h.List(r.Context())
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"workers": records})
}
func (h *Hub) Connect(w http.ResponseWriter, r *http.Request) {
	p, ok := identity.FromContext(r.Context())
	if !ok || !p.Verified || !identity.HasDeviceScope(p, identity.ScopeWorker) {
		http.Error(w, "worker device required", http.StatusForbidden)
		return
	}
	ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = ws.Close() }()
	stop := context.AfterFunc(r.Context(), func() { _ = ws.Close() })
	defer stop()
	ws.SetReadLimit(8 << 20)
	_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
	var hello workerwire.Frame
	if ws.ReadJSON(&hello) != nil || hello.T != "hello" || hello.Proto != workerwire.Protocol || hello.Worker == nil || hello.Worker.Label == "" {
		return
	}
	if len(hello.Bindings) > 0 && (!feature(hello.Features, "bind") || !feature(hello.Features, "deliver")) {
		return
	}
	sum := sha256.Sum256([]byte(p.ID))
	id := "worker_" + hex.EncodeToString(sum[:12])
	now := time.Now().UTC().Format(time.RFC3339Nano)
	caps := string(hello.Caps)
	if caps == "" {
		caps = "{}"
	}
	_, err = h.db.ExecContext(r.Context(), `INSERT INTO workers(id,label,device_id,platform,caps_json,last_seen,created_at) VALUES(?,?,?,?,?,?,?) ON CONFLICT(device_id) DO UPDATE SET label=excluded.label,platform=excluded.platform,caps_json=excluded.caps_json,last_seen=excluded.last_seen,retired_at=NULL,superseded_by=NULL`, id, hello.Worker.Label, p.ID, hello.Worker.Platform+"/"+hello.Worker.Arch, caps, now, now)
	if err != nil {
		return
	}
	c := &connection{ws: ws, pending: make(map[string]chan workerwire.Frame), inventory: make(map[string]workerwire.Session), replaying: make(map[string]bool), done: make(chan struct{}),
		ingest: make(chan workerwire.Frame, ingestQueue), control: make(chan workerwire.Frame, controlQueue), dropped: map[string]bool{}, controlDone: make(chan struct{}), ingestDone: make(chan struct{})}
	c.features = hello.Features
	c.hosts = hello.Hosts
	c.restore = hello.Restore
	if r := hello.Restore; r != nil {
		log.Printf("workerhub: worker %s restored live=%d dead_skipped=%d pruned=%d failed=%d", id, r.Live, r.DeadSkipped, r.Pruned, r.Failed)
	}
	for _, s := range hello.Sessions {
		c.inventory[s.ID] = s
	}
	h.mu.Lock()
	old := h.connections[id]
	h.connections[id] = c
	previous := h.lastIngest[id]
	h.lastIngest[id] = c.ingestDone
	h.mu.Unlock()
	if old != nil {
		_ = old.ws.Close()
	}
	// Serialized replacement must revoke the previous connection's inventory
	// before any new bind is installed. Its late defer must not revoke ours.
	h.disconnectMail(id)
	controlStarted := false
	defer func() {
		close(c.done)
		if controlStarted {
			<-c.controlDone
		} else {
			close(c.ingestDone)
		}
		h.mu.Lock()
		if h.connections[id] == c {
			delete(h.connections, id)
			h.disconnectMail(id)
		}
		h.mu.Unlock()
	}()
	welcome := workerwire.Frame{T: "welcome", WorkerID: id}
	if feature(hello.Features, "bind") && feature(hello.Features, "deliver") {
		welcome.Features = []string{"bind", "deliver"}
	}
	if feature(hello.Features, workerwire.FeatureActivity) {
		welcome.Features = append(welcome.Features, workerwire.FeatureActivity)
	}
	for _, b := range hello.Bindings {
		bound, err := h.bind(r.Context(), id, c, b)
		if err != nil {
			welcome.Error = err.Error()
			continue
		}
		welcome.Bindings = append(welcome.Bindings, bound)
	}
	lookupCtx, cancelLookup := context.WithTimeout(context.WithoutCancel(r.Context()), welcomeLookupTimeout)
	var held []string
	kills := map[string]string{}
	for _, s := range hello.Sessions {
		state, seq, err := h.heldSession(lookupCtx, s.ID, id)
		kill, reason, hold := welcomeVerdict(state, err)
		switch {
		case kill:
			welcome.Kill = append(welcome.Kill, s.ID)
			kills[s.ID] = reason
		case hold:
			held = append(held, s.ID)
			log.Printf("workerhub: worker %s holds session %s the hub could not read (%v); left running, not killed, and replayed once readable", id, s.ID, err)
		default:
			welcome.Resume = append(welcome.Resume, workerwire.Cursor{Session: s.ID, FromSeq: seq + 1})
			c.replaying[s.ID] = true
		}
	}
	cancelLookup()
	// A connection that went away during the lookups gets no welcome: the worker
	// reconnects and the next handshake decides afresh.
	if r.Context().Err() != nil {
		return
	}
	for _, s := range welcome.Kill {
		log.Printf("workerhub: worker %s: welcome orders session %s killed (%s)", id, s, kills[s])
		if h.KillOrdered != nil {
			h.KillOrdered(id, s, kills[s])
		}
	}
	if c.send(welcome) != nil {
		return
	}
	if len(held) > 0 {
		// Held sessions replay from the stored cursor once ingest is idle (replayDropped).
		c.pendingMu.Lock()
		for _, s := range held {
			c.dropped[s] = true
		}
		c.pendingMu.Unlock()
	}
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-c.done:
				return
			case <-ticker.C:
				if c.send(workerwire.Frame{T: "ping", TS: time.Now().Unix()}) != nil {
					_ = ws.Close()
					return
				}
				// last_seen from here, not per frame on the reader: a busy database must
				// not hold up response routing.
				if at := c.lastFrame.Load(); at > 0 {
					_, _ = h.db.Exec(`UPDATE workers SET last_seen=? WHERE id=?`, time.Unix(0, at).UTC().Format(time.RFC3339Nano), id)
				}
			}
		}
	}()
	controlStarted = true
	go h.runControl(r.Context(), id, c)
	go h.runIngest(id, c, previous)
	for {
		var f workerwire.Frame
		if ws.ReadJSON(&f) != nil {
			return
		}
		_ = ws.SetReadDeadline(time.Now().Add(45 * time.Second))
		c.lastFrame.Store(time.Now().UnixNano())
		switch f.T {
		case "bind", "unbind", "activity":
			// Worker requests that touch the database: off the reader, in order.
			select {
			case c.control <- f:
			case <-c.done:
				return
			}
			continue
		case "hosts":
			c.pendingMu.Lock()
			c.hosts = f.Hosts
			c.pendingMu.Unlock()
			continue
		case "state", "launched":
			c.pendingMu.Lock()
			state := f.State
			if state == "" {
				state = "running"
			}
			c.inventory[f.Session] = workerwire.Session{ID: f.Session, State: state, ExitCode: f.ExitCode}
			c.pendingMu.Unlock()
		}
		if f.CmdID != "" {
			c.pendingMu.Lock()
			ch := c.pending[f.CmdID]
			c.pendingMu.Unlock()
			if ch != nil {
				select {
				case ch <- f:
				default:
				}
			}
		}
		if f.Session != "" && (h.Event != nil || f.T == "replay_end") {
			c.enqueue(f)
		}
	}
}

// enqueue hands a session frame to the ingest goroutine without ever blocking the reader. A frame that
// does not fit is dropped, and so is every later frame of its session until ingest has drained and asked
// the worker to replay it from the stored cursor: ingest stays in order and nothing is lost.
func (c *connection) enqueue(f workerwire.Frame) {
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	if c.dropped[f.Session] {
		c.droppedFrames.Add(1)
		return
	}
	select {
	case c.ingest <- f:
	default:
		c.dropped[f.Session] = true
		c.droppedFrames.Add(1)
	}
}

// runIngest applies a connection's session frames in order (Event), asks for a replay when one fails, and,
// whenever the queue is drained, asks for a replay of every session whose frames were dropped.
func (h *Hub) runIngest(id string, c *connection, previous <-chan struct{}) {
	defer close(c.ingestDone)
	if previous != nil {
		select {
		case <-previous:
		case <-c.done:
			return
		case <-time.After(ingestHandoverWait):
			log.Printf("workerhub: worker %s: the replaced connection's ingest is still running after %s; starting anyway", id, ingestHandoverWait)
		}
	}
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case f := <-c.ingest:
			h.ingestFrame(id, c, f)
		case <-tick.C:
		}
		if len(c.ingest) == 0 {
			h.replayDropped(c)
		}
	}
}

func (h *Hub) ingestFrame(id string, c *connection, f workerwire.Frame) {
	if f.T == "replay_end" {
		_ = c.send(workerwire.Frame{T: "replay", Session: f.Session, FromSeq: h.NextSeq(f.Session)})
		return
	}
	if f.T == "state" {
		delete(c.replaying, f.Session)
	}
	if h.Event == nil {
		return
	}
	if err := h.Event(id, f); err != nil && !c.replaying[f.Session] {
		c.replaying[f.Session] = true
		_ = c.send(workerwire.Frame{T: "replay", Session: f.Session, FromSeq: h.NextSeq(f.Session)})
	}
}

func (h *Hub) replayDropped(c *connection) {
	c.pendingMu.Lock()
	sessions := make([]string, 0, len(c.dropped))
	for s := range c.dropped {
		sessions = append(sessions, s)
	}
	clear(c.dropped)
	c.pendingMu.Unlock()
	for _, s := range sessions {
		c.replaying[s] = true
		_ = c.send(workerwire.Frame{T: "replay", Session: s, FromSeq: h.NextSeq(s)})
	}
}

// runControl answers a connection's worker requests (bind, unbind, activity) in order, off the reader.
func (h *Hub) runControl(ctx context.Context, id string, c *connection) {
	defer close(c.controlDone)
	for {
		var f workerwire.Frame
		select {
		case <-c.done:
			return
		case f = <-c.control:
		}
		if f.T == "activity" {
			if feature(c.features, workerwire.FeatureActivity) {
				if err := h.receiveActivity(id, c, f.Activity, time.Now()); err != nil {
					log.Printf("worker activity rejected: %v", err)
				}
			}
			continue
		}
		response := workerwire.Frame{T: f.T + "_ack", CmdID: f.CmdID}
		if f.Binding == nil {
			response.Error = "binding required"
		} else if f.T == "bind" {
			b, err := h.bind(ctx, id, c, *f.Binding)
			if err != nil {
				response.Error = err.Error()
			} else {
				response.Binding = &b
			}
		} else if err := h.unbind(ctx, id, *f.Binding); err != nil {
			response.Error = err.Error()
		}
		if c.send(response) != nil {
			_ = c.ws.Close()
			return
		}
	}
}

func (h *Hub) hosts(id string) []workerwire.Host {
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c == nil {
		return nil
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	return append([]workerwire.Host(nil), c.hosts...)
}

// Reap asks a connected worker to terminate its orphaned shim-hosts now (no grace) and returns what it reaped.
func (h *Hub) Reap(ctx context.Context, worker string) ([]string, error) {
	id, err := h.Resolve(ctx, worker)
	if err != nil {
		return nil, err
	}
	resp, err := h.Send(ctx, id, workerwire.Frame{T: "reap", CmdID: uuid.NewString()})
	if err != nil {
		return nil, err
	}
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c != nil {
		c.pendingMu.Lock()
		c.hosts = resp.Hosts
		c.pendingMu.Unlock()
	}
	return resp.Reaped, nil
}

// ReapHTTP is POST /workers/reap {"worker": "<id or label>"}.
func (h *Hub) ReapHTTP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Worker string `json:"worker"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&in) != nil || in.Worker == "" {
		http.Error(w, "worker required", http.StatusBadRequest)
		return
	}
	reaped, err := h.Reap(r.Context(), in.Worker)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, ErrNotEnrolled) {
			code = http.StatusNotFound
		}
		http.Error(w, err.Error(), code)
		return
	}
	if reaped == nil {
		reaped = []string{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"reaped": reaped})
}

// Inventory is a connected worker's current session inventory (id to state) and whether it is known
// complete: the worker's hello carried its start-up restore counts with no failed restore
// (CW-20261114-0056), so a session it does not list is not running there. connected is false when
// the worker is not connected; the map is a copy.
func (h *Hub) Inventory(id string) (sessions map[string]string, complete, connected bool) {
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c == nil {
		return nil, false, false
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	sessions = make(map[string]string, len(c.inventory))
	for k, v := range c.inventory {
		sessions[k] = v.State
	}
	return sessions, c.restore != nil && c.restore.Failed == 0, true
}

func (h *Hub) SessionState(id, session string) (string, bool) {
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c == nil {
		return "", false
	}
	c.pendingMu.Lock()
	defer c.pendingMu.Unlock()
	s, ok := c.inventory[session]
	return s.State, ok && s.State != "exited"
}
func (h *Hub) NextSeq(session string) int64 {
	var seq int64
	_ = h.db.QueryRow(`SELECT last_seq FROM session_worker_seq WHERE session_id=?`, session).Scan(&seq)
	return seq + 1
}
func (h *Hub) Send(ctx context.Context, id string, f workerwire.Frame) (workerwire.Frame, error) {
	h.mu.RLock()
	c := h.connections[id]
	h.mu.RUnlock()
	if c == nil {
		return workerwire.Frame{}, fmt.Errorf("worker offline")
	}
	if f.T == "deliver" && !feature(c.features, "deliver") {
		return workerwire.Frame{}, fmt.Errorf("worker lacks deliver feature")
	}
	if f.CmdID == "" {
		return workerwire.Frame{}, c.send(f)
	}
	ch := make(chan workerwire.Frame, 1)
	c.pendingMu.Lock()
	c.pending[f.CmdID] = ch
	c.pendingMu.Unlock()
	defer func() { c.pendingMu.Lock(); delete(c.pending, f.CmdID); c.pendingMu.Unlock() }()
	if err := c.send(f); err != nil {
		return workerwire.Frame{}, err
	}
	select {
	case response := <-ch:
		c.timeouts.Store(0)
		if response.Error != "" {
			return response, fmt.Errorf("%s", response.Error)
		}
		return response, nil
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			h.sendTimedOut(id, c)
		}
		return workerwire.Frame{}, ctx.Err()
	case <-c.done:
		return workerwire.Frame{}, fmt.Errorf("worker disconnected")
	}
}

// sendTimedOut counts a command the worker did not answer in time. A run of DegradedAfter is reported; a
// run of ReconnectAfter closes the connection, so the worker reconnects, resumes its sessions from the
// stored cursors and starts with an empty ingest queue (CW-20261114-0104).
func (h *Hub) sendTimedOut(id string, c *connection) {
	n := int(c.timeouts.Add(1))
	action := ""
	switch n {
	case DegradedAfter:
		action = "reported"
	case ReconnectAfter:
		action = "reconnect"
	default:
		return
	}
	d := DeliveryDegraded{ConsecutiveTimeouts: n, IngestBacklog: len(c.ingest), DroppedFrames: c.droppedFrames.Load(), Action: action}
	log.Printf("workerhub: worker %s delivery degraded: %d commands unanswered in a row, ingest backlog %d, dropped frames %d; %s", id, n, d.IngestBacklog, d.DroppedFrames, action)
	if h.Degraded != nil {
		h.Degraded(id, d)
	}
	if action == "reconnect" {
		_ = c.ws.Close()
	}
}
