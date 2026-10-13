package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/agent"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/session"
	"github.com/hollis-labs/tether/internal/store"
)

// The reaper enforces lifecycle limits (max duration, lease, idle timeout) that are measured in minutes
// or hours, so it does not need to run every few seconds. At five seconds it was also the daemon's
// dominant CPU consumer on the hub (CW-20261012-0120).
const sessionReaperInterval = 5 * time.Minute
const sessionReaperEvent = "session.reaper"

// Self-limiting rules. A sweep can never become a runaway: it examines a bounded number of sessions for a
// bounded time (the rest continue on the next sweep from a rotating cursor), sessions it cannot resolve
// are backed off exponentially, and a session whose identity has not changed since it was last examined
// is skipped without reading its events at all.
const (
	reaperMaxPerSweep = 64
	reaperSweepBudget = 30 * time.Second
	reaperSlowSweep   = 5 * time.Second
	reaperBackoffBase = 10 * time.Minute
	reaperBackoffMax  = 6 * time.Hour
)

type sessionReaper struct {
	cancel context.CancelFunc
	done   chan struct{}
	sweep  sync.Mutex
	stops  sync.Map
	cursor int // rotating start index, guarded by sweep
	// lastExamined and lastSkipped describe the previous sweep (guarded by sweep; read by tests).
	lastExamined, lastSkipped int
	backoff                   sync.Map // session id -> reaperBackoff
}

// reaperBackoff records that a session was examined and left unresolved (a retained shim, a live
// process of unknown identity). It applies only while the session's identity signature is unchanged.
type reaperBackoff struct {
	sig   string
	fails int
	until time.Time
}

// reaperBackoffFor is 10m, 20m, 40m ... capped at 6h.
func reaperBackoffFor(fails int) time.Duration {
	if fails < 1 {
		fails = 1
	}
	d := reaperBackoffBase
	for i := 1; i < fails && d < reaperBackoffMax; i++ {
		d *= 2
	}
	if d > reaperBackoffMax {
		d = reaperBackoffMax
	}
	return d
}

// reaperSignature identifies a session's observable lifecycle identity. updated_at is deliberately
// excluded: recovery bookkeeping rewrites it without changing anything the reaper cares about.
func reaperSignature(row store.ReaperSession) string {
	return fmt.Sprintf("%s|%d|%s|%s", row.State, row.PID.Int64, row.PIDStartedAt, row.CreatedAt)
}

func (r *sessionReaper) backedOff(id, sig string, now time.Time) bool {
	v, ok := r.backoff.Load(id)
	if !ok {
		return false
	}
	b := v.(reaperBackoff)
	return b.sig == sig && now.Before(b.until)
}

func (r *sessionReaper) park(id, sig string, now time.Time) {
	fails := 1
	if v, ok := r.backoff.Load(id); ok {
		if b := v.(reaperBackoff); b.sig == sig {
			fails = b.fails + 1
		}
	}
	r.backoff.Store(id, reaperBackoff{sig: sig, fails: fails, until: now.Add(reaperBackoffFor(fails))})
}

func (r *sessionReaper) clear(id string) { r.backoff.Delete(id) }

type reaperStop struct {
	done chan struct{}
	err  error
}

func (s *Service) reaperState() *sessionReaper {
	s.reaperMu.Lock()
	defer s.reaperMu.Unlock()
	if s.reaper == nil {
		s.reaper = &sessionReaper{}
	}
	return s.reaper
}

// StartSessionReaper starts one cancellable loop after startup recovery. Limits
// come from immutable resolved launch plans; a zero/unset limit never kills.
func (s *Service) StartSessionReaper(parent context.Context) {
	s.reaperMu.Lock()
	defer s.reaperMu.Unlock()
	if s.reaper == nil {
		s.reaper = &sessionReaper{}
	}
	r := s.reaper
	if r.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel = cancel
	r.done = make(chan struct{})
	go func() {
		defer close(r.done)
		ticker := time.NewTicker(sessionReaperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				if err := s.SweepSessionReaper(ctx, now); err != nil && ctx.Err() == nil {
					log.Printf("session reaper: %v", err)
				}
			}
		}
	}()
}

// StopSessionReaper joins the worker before the runtime or store is closed.
func (s *Service) StopSessionReaper() {
	s.reaperMu.Lock()
	r := s.reaper
	if r == nil || r.cancel == nil {
		s.reaperMu.Unlock()
		return
	}
	r.cancel()
	done := r.done
	s.reaperMu.Unlock()
	<-done
}

func reaperReason(now, started, activity time.Time, d launchprofile.LifecycleDurations, leaseExpired bool) string {
	if d.MaxDuration > 0 && !now.Before(started.Add(d.MaxDuration)) {
		return "max_duration"
	}
	if leaseExpired || d.LeaseDuration > 0 && !now.Before(started.Add(d.LeaseDuration)) {
		return "lease_expired"
	}
	if activity.Before(started) {
		activity = started
	}
	if d.IdleTimeout > 0 && !now.Before(activity.Add(d.IdleTimeout)) {
		return "idle_timeout"
	}
	return ""
}

// SweepSessionReaper never runs the daemon-start bulk sweep against live
// sessions. Manager-owned PID-zero turn runtimes are healthy candidates.
func (s *Service) SweepSessionReaper(ctx context.Context, now time.Time) error {
	r := s.reaperState()
	r.sweep.Lock()
	defer r.sweep.Unlock()
	began := time.Now()
	rows, err := s.Store.ReaperCandidates(ctx)
	if err != nil {
		return err
	}
	var failures []error
	examined, skipped := 0, 0
	n := len(rows)
	start := 0
	if n > 0 {
		start = r.cursor % n
	}
	truncated := false
	for i := 0; i < n; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if examined >= reaperMaxPerSweep || time.Since(began) > reaperSweepBudget {
			r.cursor = (start + i) % n
			truncated = true
			break
		}
		row := rows[(start+i)%n]
		if session.State(row.State).Terminal() || row.State == string(session.StateOrphaned) {
			r.clear(row.ID)
			continue
		}
		sig := reaperSignature(row)
		if r.backedOff(row.ID, sig, now) {
			skipped++
			continue
		}
		examined++
		if err := s.Store.ReaperTimes(ctx, &row); err != nil {
			failures = append(failures, err)
			continue
		}
		plan, err := s.Store.GetLaunchPlan(row.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		d, err := plan.Lifecycle.Durations()
		if err != nil {
			failures = append(failures, err)
			continue
		}
		started := row.StartedAt
		if started.IsZero() {
			failures = append(failures, fmt.Errorf("session %s has no valid start time", row.ID))
			continue
		}
		// Launch startup is allowed its grace, including the interval before a
		// Manager handle or process start-time stamp has been installed.
		if row.State == string(session.StateLaunching) && now.Before(started.Add(d.OrphanGrace)) {
			continue
		}
		live := false
		if s.Manager != nil {
			_, live = s.Manager.Get(row.ID)
		}
		if !live && !now.Before(started.Add(d.OrphanGrace)) {
			stale := store.StaleSession{ID: row.ID, State: row.State, PID: int(row.PID.Int64), CreatedAt: row.CreatedAt, PIDStartedAt: row.PIDStartedAt}
			if s.reconcileShimContext(ctx, stale) { // provider owns identity and outcome
				r.park(row.ID, sig, now)
				continue
			}
			procs := s.procs
			if procs == nil {
				procs = osProcessInspector{}
			}
			if row.PID.Valid && row.PID.Int64 > 0 && procs.alive(int(row.PID.Int64)) && !sessionProcessZombie(int(row.PID.Int64)) {
				// Unknown identity is retained, never signaled or reclassified on a
				// transient inspection failure. Verified reused PIDs are orphans.
				actual, known := procs.startTime(int(row.PID.Int64))
				if !known || row.PIDStartedAt == "" || actual == row.PIDStartedAt {
					r.park(row.ID, sig, now)
					continue
				}
			}
			changed, err := s.Store.RecordReaperOrphan(ctx, row.ID, "process_missing", row.SessionRow)
			if err != nil {
				failures = append(failures, err)
			} else if changed {
				r.clear(row.ID)
				s.publishSessionStateChange(store.SessionStateChange{SessionID: row.ID, LogicalAgentID: row.LogicalAgentID, From: row.State, To: string(session.StateOrphaned), Reason: "process_missing"})
			}
			continue
		}
		r.clear(row.ID)
		activity := row.LastActivity
		if at := s.SessionLastActivity(row.ID); at.After(activity) {
			if err := s.Store.RecordSessionActivity(ctx, row.ID, at); err != nil {
				failures = append(failures, err)
				continue
			}
			activity = at
		}
		expired, err := s.Store.SessionLeaseExpired(ctx, row.ID, now)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		reason := reaperReason(now, started, activity, d, expired)
		if reason != "" {
			if err := s.stopSessionForLifecycle(ctx, row.ID, reason, d); err != nil {
				failures = append(failures, fmt.Errorf("%s: %w", row.ID, err))
			}
		}
	}
	if !truncated {
		r.cursor = 0
	}
	r.lastExamined, r.lastSkipped = examined, skipped
	if took := time.Since(began); took > reaperSlowSweep || truncated {
		log.Printf("session reaper: slow or truncated sweep: candidates=%d examined=%d skipped_backoff=%d truncated=%t took=%s", n, examined, skipped, truncated, took.Round(time.Millisecond))
	}
	return errors.Join(failures...)
}

func (s *Service) reaperEvent(ctx context.Context, row *store.SessionRow, reason, stage, outcome string, err error) error {
	payload := map[string]string{"reason": reason, "stage": stage, "outcome": outcome}
	if err != nil {
		payload["error"] = err.Error()
	}
	raw, _ := json.Marshal(payload)
	if s.Bus == nil {
		_, _, err := s.Store.InsertEventContext(ctx, events.ScopeSession, row.ID, sessionReaperEvent, string(raw))
		return err
	}
	return s.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: row.ID, LogicalAgentID: row.LogicalAgentID, Kind: sessionReaperEvent, PayloadJSON: string(raw)})
}

func (s *Service) stopSessionWithLifecycle(id string) error {
	plan, err := s.Store.GetLaunchPlan(id)
	if errors.Is(err, store.ErrSessionNotFound) || errors.Is(err, sql.ErrNoRows) {
		return agentsessions.ErrSessionNotRunning
	}
	if err != nil {
		return err
	}
	d, err := plan.Lifecycle.Durations()
	if err != nil {
		return err
	}
	return s.stopSessionForLifecycle(context.Background(), id, "user_stop", d)
}

func (s *Service) stopSessionForLifecycle(ctx context.Context, id, reason string, d launchprofile.LifecycleDurations) (result error) {
	r := s.reaperState()
	op := &reaperStop{done: make(chan struct{})}
	existing, loaded := r.stops.LoadOrStore(id, op)
	if loaded {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-existing.(*reaperStop).done:
			return existing.(*reaperStop).err
		}
	}
	defer func() { op.err = result; close(op.done); r.stops.Delete(id) }()
	row, err := s.Store.GetSession(id)
	if err != nil {
		return err
	}
	if session.State(row.State).Terminal() || row.State == string(session.StateOrphaned) {
		return agentsessions.ErrSessionNotRunning
	}
	// The intent must be durable before any signal. An unavailable event store
	// refuses teardown rather than letting a session disappear silently.
	if err = s.reaperEvent(ctx, row, reason, "requested", "", nil); err != nil {
		return err
	}
	s.stops.markWithReason(id, reason)
	defer s.stops.clear(id)
	defer func() {
		outCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		current, gerr := s.Store.GetSession(id)
		outcome := "retained"
		if gerr == nil {
			outcome = current.State
		}
		result = errors.Join(result, s.reaperEvent(outCtx, row, reason, "outcome", outcome, result))
	}()
	// Shim placements must use the canonical identity-checked host stop path.
	// It already requests stop and escalates TERM/KILL with host-owned grace.
	if _, err = s.Store.SessionShim(ctx, id); err == nil {
		return s.finishLifecycleRuntimeStop(ctx, id, d)
	} else if !errors.Is(err, store.ErrSessionShimNotFound) {
		return err
	}
	if s.Manager == nil {
		return agentsessions.ErrSessionNotRunning
	}
	health, managed := s.Manager.Health(id)
	if !managed {
		return agentsessions.ErrSessionNotRunning
	}
	// PID-zero/API runtimes use their provider cancellation contract; a PID
	// retained from a previous subprocess turn must never be signaled.
	if health.Health.PID <= 0 || health.RuntimeKind == "api" {
		return s.finishLifecycleRuntimeStop(ctx, id, d)
	}
	if row.LogicalAgentID != "" {
		policy, err := s.Store.GetLogicalAgentPolicy(row.LogicalAgentID)
		if err != nil {
			return err
		}
		if policy.CheckpointPolicy == agent.CheckpointPolicyOnStop {
			if err := s.CreatePolicyCheckpoint(row, policy); err != nil {
				return err
			}
		}
	}
	return s.stopVerifiedSessionProcess(ctx, row, health.Health.PID, reason, d)
}

// A provider Stop may return before its watcher commits the terminal row.
// Join it before reporting a successful durable outcome.
func (s *Service) finishLifecycleRuntimeStop(ctx context.Context, id string, d launchprofile.LifecycleDurations) error {
	if err := s.stopSessionImmediate(id); err != nil {
		return err
	}
	if s.Manager == nil {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, max(d.KillGrace, time.Millisecond))
	defer cancel()
	return s.waitLifecycleExit(waitCtx, id)
}

func (s *Service) waitLifecycleExit(ctx context.Context, id string) error {
	_, err := s.Manager.WaitSession(ctx, id)
	if err == nil || errors.Is(err, agentsessions.ErrSessionNotRunning) {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	// A signal-induced Wait error is an observed exit, not a failed teardown.
	row, readErr := s.Store.GetSession(id)
	if readErr == nil && session.State(row.State).Terminal() {
		return nil
	}
	return errors.Join(err, readErr)
}
