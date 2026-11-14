package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/hollis-labs/tether/internal/store"
)

// Ghost cleanup (CW-20261114-0007). A ghost is a row that claims something is alive when it is not:
//
//	(a) worker_missing / worker_exited: an open session on a CONNECTED worker that the worker's live
//	    inventory does not hold (or holds as exited), past the launch grace. Only a worker whose
//	    hello proved its inventory complete (CW-20261114-0056 restore counts, no failed restore) is
//	    trusted: on any other the row is reported and the apply refuses it.
//	(b) worker_gone: an open session on a worker that is not connected and not seen for WorkerStale.
//	(c) hub_orphaned: a hub-local session the reaper already marked orphaned, with no managed runtime,
//	    no live process and no live shim. (Hub-local launching/running rows are the reaper's.)
//	(d) dead custody: a session_shims row of an ended session with no live host, shim or provider pid
//	    and no control socket, not frozen by a replacement.
//	(e) stale worker: a worker row not seen for WorkerStale with no open session. Retired (hidden,
//	    superseded_by the newest live row of its label) when anything references it; deleted only when
//	    nothing does (no session, mail grant or event).
//
// A dry run changes nothing. An apply needs the path of a backup taken first; each write re-checks the
// row it was classified from.

// GhostReapOptions selects a dry run or an apply.
type GhostReapOptions struct {
	Apply bool `json:"apply"`
	// Backup is the path of the database backup taken before an apply; it must exist and be non-empty.
	Backup string `json:"backup,omitempty"`
}

const (
	ghostLaunchGrace = 10 * time.Minute
	ghostWorkerStale = 7 * 24 * time.Hour
)

// GhostSession is one ghost session row and what was (or would be) done.
type GhostSession struct {
	ID             string `json:"id"`
	Worker         string `json:"worker,omitempty"`
	State          string `json:"state"`
	LogicalAgentID string `json:"logical_agent_id,omitempty"`
	UpdatedAt      string `json:"updated_at"`
	Class          string `json:"class"`
	Bindings       int    `json:"active_bindings"`
	// Action is "close" or "blocked: <why>"; Done is true once an apply closed it.
	Action string `json:"action"`
	Done   bool   `json:"done,omitempty"`
}

// GhostCustody is one dead hub-local shim custody row.
type GhostCustody struct {
	SessionID string `json:"session_id"`
	State     string `json:"state"`
	Action    string `json:"action"`
	Done      bool   `json:"done,omitempty"`
}

// GhostWorker is one stale worker row.
type GhostWorker struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	LastSeen     string `json:"last_seen"`
	Sessions     int    `json:"sessions"`
	Open         int    `json:"open_sessions"`
	Grants       int    `json:"mail_grants"`
	InEvents     *bool  `json:"in_events,omitempty"`
	SupersededBy string `json:"superseded_by,omitempty"`
	// Action is "retire", "delete" or "keep: <why>".
	Action string `json:"action"`
	Done   bool   `json:"done,omitempty"`
}

// GhostCounts summarizes a classification.
type GhostCounts struct {
	OpenSessions   int `json:"open_sessions"`
	GhostSessions  int `json:"ghost_sessions"`
	GhostBindings  int `json:"ghost_active_bindings"`
	DeadCustody    int `json:"dead_custody"`
	StaleWorkers   int `json:"stale_workers"`
	BlockedGhosts  int `json:"blocked_ghost_sessions"`
	WorkersInScope int `json:"workers_in_scope"`
}

// GhostReport is the result of ReapGhosts.
type GhostReport struct {
	Applied  bool           `json:"applied"`
	Backup   string         `json:"backup,omitempty"`
	Sessions []GhostSession `json:"sessions"`
	Custody  []GhostCustody `json:"custody"`
	Workers  []GhostWorker  `json:"workers"`
	Before   GhostCounts    `json:"before"`
	// After is the classification re-run after an apply; absent on a dry run.
	After *GhostCounts `json:"after,omitempty"`
	open  int
}

// ErrGhostBackupRequired refuses an apply without a usable backup path.
var ErrGhostBackupRequired = errors.New("an apply needs --backup naming the database backup taken first")

// ghostRequestError is a caller mistake: the HTTP surface answers 400.
type ghostRequestError struct{ error }

func (ghostRequestError) BadRequest() bool { return true }
func (e ghostRequestError) Unwrap() error  { return e.error }

// ReapGhostsReport is ReapGhosts for the daemon's POST /ghosts/reap.
func (s *Service) ReapGhostsReport(ctx context.Context, apply bool, backup string) (any, error) {
	report, err := s.ReapGhosts(ctx, GhostReapOptions{Apply: apply, Backup: backup})
	if err != nil {
		return nil, err
	}
	return report, nil
}

// ReapGhosts classifies ghost sessions, dead shim custody and stale workers and, with Apply, cleans
// them up.
func (s *Service) ReapGhosts(ctx context.Context, opts GhostReapOptions) (*GhostReport, error) {
	if opts.Apply {
		fi, err := os.Stat(opts.Backup)
		if opts.Backup == "" || err != nil || !fi.Mode().IsRegular() || fi.Size() == 0 {
			return nil, ghostRequestError{fmt.Errorf("%w (got %q)", ErrGhostBackupRequired, opts.Backup)}
		}
	}
	now := time.Now().UTC()
	report, err := s.classifyGhosts(ctx, now)
	if err != nil {
		return nil, err
	}
	report.Before = report.counts()
	if !opts.Apply {
		return report, nil
	}
	report.Applied, report.Backup = true, opts.Backup
	byID := map[string]store.OpenSession{}
	open, err := s.Store.OpenSessions(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range open {
		byID[o.ID] = o
	}
	for i := range report.Sessions {
		g := &report.Sessions[i]
		row, ok := byID[g.ID]
		if g.Action != "close" || !ok || row.UpdatedAt != g.UpdatedAt {
			continue
		}
		if g.Done, err = s.Store.CloseGhostSession(ctx, row, "ghost:"+g.Class); err != nil {
			return report, fmt.Errorf("close ghost %s: %w", g.ID, err)
		}
	}
	// Custody of sessions closed above is classified again now that they have ended.
	custody, err := s.deadCustody(ctx)
	if err != nil {
		return report, err
	}
	report.Custody = custody
	for i := range report.Custody {
		c := &report.Custody[i]
		if c.Action != "delete" {
			continue
		}
		if c.Done, err = s.Store.DeleteShimCustody(ctx, c.SessionID); err != nil {
			return report, fmt.Errorf("delete custody %s: %w", c.SessionID, err)
		}
	}
	// The workers were classified with this run's closes discounted; RetireWorker and DeleteWorker
	// re-check open sessions, sessions and grants, so the events scan is not repeated here.
	for i := range report.Workers {
		w := &report.Workers[i]
		switch w.Action {
		case "retire":
			w.Done, err = s.Store.RetireWorker(ctx, w.ID, w.SupersededBy, w.LastSeen)
		case "delete":
			w.Done, err = s.Store.DeleteWorker(ctx, w.ID, w.LastSeen)
		}
		if err != nil {
			return report, fmt.Errorf("%s worker %s: %w", w.Action, w.ID, err)
		}
	}
	after, err := s.classifyGhosts(ctx, time.Now().UTC())
	if err != nil {
		return report, err
	}
	c := after.counts()
	report.After = &c
	return report, nil
}

func (r *GhostReport) counts() GhostCounts {
	c := GhostCounts{OpenSessions: r.open}
	for _, g := range r.Sessions {
		c.GhostSessions++
		c.GhostBindings += g.Bindings
		if g.Action != "close" {
			c.BlockedGhosts++
		}
	}
	for _, d := range r.Custody {
		if d.Action == "delete" || d.Action == "delete after close" {
			c.DeadCustody++
		}
	}
	for _, w := range r.Workers {
		c.WorkersInScope++
		if w.Action == "retire" || w.Action == "delete" {
			c.StaleWorkers++
		}
	}
	return c
}

// classifyGhosts builds a report without changing anything.
func (s *Service) classifyGhosts(ctx context.Context, now time.Time) (*GhostReport, error) {
	open, err := s.Store.OpenSessions(ctx)
	if err != nil {
		return nil, err
	}
	report := &GhostReport{Sessions: []GhostSession{}, Custody: []GhostCustody{}, Workers: []GhostWorker{}, open: len(open)}
	lastSeen, err := s.workerLastSeen(ctx)
	if err != nil {
		return nil, err
	}
	type inventory struct {
		sessions            map[string]string
		complete, connected bool
	}
	inventories := map[string]inventory{}
	closing := map[string]bool{}
	for _, o := range open {
		class, action := s.ghostClass(o, now, lastSeen, func(worker string) (map[string]string, bool, bool) {
			inv, ok := inventories[worker]
			if !ok && s.Workers != nil {
				inv.sessions, inv.complete, inv.connected = s.Workers.Inventory(worker)
				inventories[worker] = inv
			}
			return inv.sessions, inv.complete, inv.connected
		})
		if class == "" {
			continue
		}
		n, err := s.Store.ActiveBindings(ctx, o.ID)
		if err != nil {
			return nil, err
		}
		report.Sessions = append(report.Sessions, GhostSession{ID: o.ID, Worker: o.WorkerID, State: o.State, LogicalAgentID: o.LogicalAgentID, UpdatedAt: o.UpdatedAt, Class: class, Bindings: n, Action: action})
		if action == "close" {
			closing[o.ID] = true
		}
	}
	if report.Custody, err = s.deadCustody(ctx); err != nil {
		return nil, err
	}
	// Custody of a ghost about to be closed is dead too once it is: show it.
	for _, g := range report.Sessions {
		if !closing[g.ID] {
			continue
		}
		if dead, ok, err := s.ghostShimDead(ctx, g.ID); err != nil {
			return nil, err
		} else if ok && dead {
			report.Custody = append(report.Custody, GhostCustody{SessionID: g.ID, State: g.State, Action: "delete after close"})
		}
	}
	if report.Workers, err = s.staleWorkers(ctx, now, closing); err != nil {
		return nil, err
	}
	return report, nil
}

// ghostClass returns the ghost class of an open session and the action, or "" when it is not a ghost.
func (s *Service) ghostClass(o store.OpenSession, now time.Time, lastSeen map[string]time.Time, inventory func(string) (map[string]string, bool, bool)) (string, string) {
	created, _ := time.Parse(time.RFC3339Nano, o.CreatedAt)
	if o.WorkerID == "" {
		if o.State != "orphaned" {
			return "", "" // the session reaper owns hub-local launching/running/detached rows
		}
		if s.Manager != nil {
			if _, live := s.Manager.Get(o.ID); live {
				return "", ""
			}
		}
		if o.PID > 0 && s.inspector().alive(int(o.PID)) && !sessionProcessZombie(int(o.PID)) {
			if actual, known := s.inspector().startTime(int(o.PID)); !known || o.PIDStartedAt == "" || actual == o.PIDStartedAt {
				return "", ""
			}
		}
		if dead, ok, _ := s.ghostShimDead(context.Background(), o.ID); ok && !dead {
			return "", ""
		}
		return "hub_orphaned", "close"
	}
	sessions, complete, connected := inventory(o.WorkerID)
	if !connected {
		seen, known := lastSeen[o.WorkerID]
		if known && now.Sub(seen) < ghostWorkerStale {
			return "", "" // offline for now; it may come back with the session
		}
		return "worker_gone", "close"
	}
	if now.Sub(created) < ghostLaunchGrace {
		return "", ""
	}
	class := "worker_missing"
	if state, held := sessions[o.ID]; held {
		if state != "exited" {
			return "", ""
		}
		class = "worker_exited"
	}
	if !complete {
		return class, "blocked: the worker's hello does not prove its inventory complete (it predates CW-20261114-0056 or a restore failed)"
	}
	return class, "close"
}

func (s *Service) inspector() processInspector {
	if s.procs != nil {
		return s.procs
	}
	return osProcessInspector{}
}

func (s *Service) workerLastSeen(ctx context.Context) (map[string]time.Time, error) {
	rows, err := s.Store.DB().QueryContext(ctx, `SELECT id,last_seen FROM workers`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var id, seen string
		if err := rows.Scan(&id, &seen); err != nil {
			return nil, err
		}
		out[id], _ = time.Parse(time.RFC3339Nano, seen)
	}
	return out, rows.Err()
}

// deadCustody classifies the shim custody rows of ended sessions.
func (s *Service) deadCustody(ctx context.Context) ([]GhostCustody, error) {
	rows, err := s.Store.EndedShimCustody(ctx)
	if err != nil {
		return nil, err
	}
	out := []GhostCustody{}
	for _, c := range rows {
		g := GhostCustody{SessionID: c.SessionID, State: c.State, Action: "delete"}
		switch {
		case c.Frozen:
			g.Action = "keep: frozen by a session replacement"
		case !s.custodyDead(c):
			g.Action = "keep: a host, shim or provider pid is alive or the control socket exists"
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Service) custodyDead(c store.ShimCustody) bool {
	for _, pid := range []int64{c.HostPID, c.ShimPID, c.ProviderPID} {
		if pid > 1 && s.inspector().alive(int(pid)) && !sessionProcessZombie(int(pid)) {
			return false
		}
	}
	if fi, err := os.Stat(c.SocketPath); c.SocketPath != "" && err == nil && fi.Mode()&os.ModeSocket != 0 {
		return false
	}
	return true
}

// ghostShimDead reports whether a session's shim custody (if it has one) is dead; ok is false when the
// session has no custody row.
func (s *Service) ghostShimDead(ctx context.Context, sessionID string) (dead, ok bool, err error) {
	var c store.ShimCustody
	err = s.Store.DB().QueryRowContext(ctx, `SELECT session_id,socket_path,host_pid,shim_pid,provider_pid FROM session_shims WHERE session_id=?`, sessionID).
		Scan(&c.SessionID, &c.SocketPath, &c.HostPID, &c.ShimPID, &c.ProviderPID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, false, nil
		}
		return false, false, err
	}
	return s.custodyDead(c), true, nil
}

// staleWorkers classifies worker rows not seen for ghostWorkerStale. closing names ghost sessions a dry
// run would close: they are not counted as open. Events are scanned (events.scope_worker_id has no
// index) only for a worker nothing else references, so only the few delete candidates pay for it.
func (s *Service) staleWorkers(ctx context.Context, now time.Time, closing map[string]bool) ([]GhostWorker, error) {
	rows, err := s.Store.StaleWorkers(ctx, now.Add(-ghostWorkerStale))
	if err != nil {
		return nil, err
	}
	out := []GhostWorker{}
	for _, w := range rows {
		if s.Workers != nil && s.Workers.Connected(w.ID) {
			continue
		}
		g := GhostWorker{ID: w.ID, Label: w.Label, LastSeen: w.LastSeen, Sessions: w.Sessions, Open: w.Open, Grants: w.Grants, SupersededBy: w.SupersededBy}
		open := w.Open
		if open > 0 && len(closing) > 0 {
			open -= s.closingOn(ctx, w.ID, closing)
		}
		switch {
		case open > 0:
			g.Action = fmt.Sprintf("keep: %d open session(s)", open)
		case w.Sessions > 0 || w.Grants > 0:
			g.Action = "retire"
		default:
			inEvents, err := s.Store.WorkerInEvents(ctx, w.ID)
			if err != nil {
				return nil, err
			}
			g.InEvents = &inEvents
			g.Action = "delete"
			if inEvents {
				g.Action = "retire"
			}
		}
		out = append(out, g)
	}
	return out, nil
}

func (s *Service) closingOn(ctx context.Context, worker string, closing map[string]bool) int {
	open, err := s.Store.OpenSessions(ctx)
	if err != nil {
		return 0
	}
	n := 0
	for _, o := range open {
		if o.WorkerID == worker && closing[o.ID] {
			n++
		}
	}
	return n
}
