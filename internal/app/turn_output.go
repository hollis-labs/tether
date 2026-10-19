package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/agentsessions"
	gop "github.com/hollis-labs/libs/harness/core/adapters/provider"
	gopevents "github.com/hollis-labs/libs/harness/core/adapters/provider/events"
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/libs/harness/core/adapters/turnoutput"
	messaging "github.com/hollis-labs/libs/mesh/core/messaging"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

// The outer lock keeps reduction, persistence and emission ordered across
// callback/termination races; the raw feeds are synchronous and lossless.
type sessionTurnOutput struct {
	providerResultID         string
	freshConversationPending bool
	freshConversationTurn    string
	lastActivity             time.Time
	runtimeOrigins           map[string]*outputEventOrigin
	runtimeOriginOrder       []string
	runtimePendingFloor      *outputEventOrigin
	runtimeIncomingOrigin    *outputEventOrigin
	submissionGate           sync.Mutex
	accepted                 bool
	submissions              int
	activity                 uint64
	unboundTerminal          *emptyTurnTerminal
	unboundAmbiguous         bool
	routeUnread              bool
	completed                map[string]string
	completedDetails         map[string]TurnCompletion
	completedOrder           []string
	mu                       sync.Mutex
	runtimeID                string
	reducer                  *turnoutput.Reducer
	service                  *Service
	row                      store.SessionRow
	route                    *launchprofile.Route
	turnID                   string
	reducerTurnID            string
	turnDone                 chan struct{}
	finishedTurns            []string
	turnStartedAt            map[string]time.Time
	// finalTurnGrace is lifecycle.stop_after_final_turn; zero means opt out.
	finalTurnGrace time.Duration
}

func (s *Service) newSessionTurnOutput(row store.SessionRow, plan *launch.Plan) *sessionTurnOutput {
	// Some hosted protocol runtimes do not consume StartOptions.LogPath. Tail
	// should still read a private empty log while real output is pending.
	if row.Workspace != "" {
		path := filepath.Join(row.Workspace, "logs", "session.log")
		if f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err != nil { //nolint:gosec // Workspace is the daemon-owned persisted session directory.
			log.Printf("ERROR session %q: initialize output log: %v", row.ID, err)
		} else if err := f.Close(); err != nil {
			log.Printf("ERROR session %q: close initialized output log: %v", row.ID, err)
		}
	}
	ctx, cancel := s.outputPersistenceContext()
	defer cancel()
	route, err := s.Store.SessionRoute(ctx, row.ID)
	if err != nil {
		log.Printf("ERROR session %q: read output route; full-text routing unavailable: %v", row.ID, err)
	}
	out := &sessionTurnOutput{service: s, row: row, route: route, routeUnread: err != nil, runtimeID: config.CanonicalRuntimeID(plan.ProviderBrand)}
	if plan.Lifecycle != nil {
		if d, derr := plan.Lifecycle.Durations(); derr == nil {
			out.finalTurnGrace = d.StopAfterFinalTurn
		}
	}
	out.reducer = turnoutput.New(turnoutput.Config{SessionID: row.ID, Runtime: config.CanonicalRuntimeID(plan.ProviderBrand), QuestionTools: s.questionTools(config.CanonicalRuntimeID(plan.ProviderBrand)), NewTurnID: func() string {
		out.ensureTurn()
		out.noteTurnActivity()
		out.reducerTurnID = out.turnID
		if out.runtimePendingFloor != nil {
			origin := outputEventOrigin{}
			if out.runtimeIncomingOrigin != nil {
				origin = *out.runtimeIncomingOrigin
			}
			origin.Envelope.TurnID = out.turnID
			out.rememberRuntimeOrigin(out.turnID, &origin)
		}
		out.accepted = true
		return out.turnID
	}})
	return out
}

func (o *sessionTurnOutput) observeProvider(ev gopevents.Event) {
	if _, denied := ev.(gopevents.PermissionDenied); denied && o.service.turnFeeds[o.runtimeID].approval == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch ev.(type) {
	case gopevents.Delta, gopevents.ToolUse, gopevents.ToolResult, gopevents.Thinking,
		gopevents.SubagentSpawn, gopevents.PermissionDenied, gopevents.Done, gopevents.Error:
		o.noteTurnActivity()
	}
	if _, lost := ev.(gopevents.SessionLost); lost {
		o.markSessionLost()
	}
	if result, ok := o.reducer.ObserveProvider(ev); ok {
		o.publish(result)
		o.completeTurn(result, false)
	} else {
		switch terminal := ev.(type) {
		case gopevents.Done:
			o.emptyTerminal(turnoutput.KindFinal, terminal.StopReason)
		case gopevents.Error:
			o.emptyTerminal(turnoutput.KindFailure, "")
		}
	}
}

func (o *sessionTurnOutput) observeRuntime(ev runtimeevents.Event) {
	registration := o.service.turnFeeds[o.runtimeID]
	if ev.Kind == runtimeevents.KindAgentPermissionDenied && registration.approval == nil {
		return
	}
	if ev.Kind == runtimeevents.KindAgentPermissionRequested || ev.Kind == runtimeevents.KindAgentPermissionResolved {
		var permission struct {
			Method string `json:"method"`
		}
		if json.Unmarshal(ev.Payload, &permission) == nil && strings.HasSuffix(strings.ToLower(permission.Method), "requestuserinput") {
			if len(registration.questionTools) == 0 {
				return
			}
		} else if registration.approval == nil && !registration.requestSignals {
			return
		}
	}
	origin, err := derivedRuntimeOrigin(ev)
	if err != nil {
		log.Printf("session %q: reject runtime envelope: %v", o.row.ID, err)
		return
	}
	o.recordEnvironmentRequest(ev, origin)
	o.mu.Lock()
	defer o.mu.Unlock()
	if ev.Kind == runtimeevents.KindSessionLost {
		o.markSessionLost()
	}
	if ev.TurnID != "" && slices.Contains(o.finishedTurns, ev.TurnID) {
		return
	}
	o.meterTurn(ev)
	switch ev.Kind {
	case runtimeevents.KindTurnStarted, runtimeevents.KindAgentDelta, runtimeevents.KindAgentToolUse,
		runtimeevents.KindAgentToolResult, runtimeevents.KindAgentSubagentSpawn, runtimeevents.KindAgentPermissionRequested,
		runtimeevents.KindAgentPermissionResolved, runtimeevents.KindAgentPermissionDenied,
		runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
		o.noteTurnActivity()
		if ev.TurnID != "" && (o.reducerTurnID == "" || ev.Kind == runtimeevents.KindTurnStarted || ev.TurnID == o.reducerTurnID) {
			o.bindTurn(ev.TurnID)
		}
		if ev.TurnID == "" {
			// The reducer owns untagged-frame attribution. A floor cannot be
			// guessed from our submission marker or a stale origin entry.
			o.carryRuntimeFloor(origin)
		} else {
			o.rememberRuntimeOrigin(ev.TurnID, origin)
		}
	default:
		// Process/session telemetry does not open a turn.
	}
	o.runtimeIncomingOrigin = origin
	result, reduced := o.reducer.Observe(ev)
	o.runtimeIncomingOrigin = nil
	if reduced {
		if ev.TurnID == "" {
			origin.Envelope.TurnID = result.TurnID
			o.rememberRuntimeOrigin(result.TurnID, origin)
		}
		o.publish(result)
		o.completeTurn(result, false)
	} else if ev.Kind == runtimeevents.KindTurnCompleted || ev.Kind == runtimeevents.KindTurnFailed {
		var terminal struct {
			StopReason string `json:"stop_reason"`
		}
		_ = json.Unmarshal(ev.Payload, &terminal)
		kind := turnoutput.KindFinal
		if ev.Kind == runtimeevents.KindTurnFailed {
			kind = turnoutput.KindFailure
		}
		o.emptyTerminal(kind, terminal.StopReason)
	}
}

func (o *sessionTurnOutput) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if result, ok := o.reducer.Flush("process_exited"); ok {
		o.publish(result)
		o.completeTurn(result, true)
	}
	// A process can end before its first reduced event.
	o.settleTurn()
	if len(o.runtimeOrigins) == 0 {
		o.runtimePendingFloor = nil
	}
	// The session is gone: queued replies hand off or become undeliverable.
	o.service.notifyReplyIdle(o.row.ID)
}

// wire is used for every native runtime. ACP's wrapper supplies runtimeevents
// at its Activity sink instead of a lossy provider EventFanout subscription.
func (o *sessionTurnOutput) wire(rt agentsessions.Runtime, opts *agentsessions.StartOptions) {
	if observer, ok := rt.(interface {
		SetEventObserver(func(runtimeevents.Event))
	}); ok {
		observer.SetEventObserver(o.observeRuntime)
		return
	}
	existing := opts.TypedEventCallback
	var permission gop.EventsCallback
	if o.runtimeID == "antigravity" || o.service.turnFeeds[o.runtimeID].approval != nil {
		denied := makeProviderTypedEventCallback(o.service.Bus, o.row.ID, o.row.LogicalAgentID)
		permission = func(ev gopevents.Event) {
			denied(ev)
			if _, ok := ev.(gopevents.PermissionDenied); ok {
				o.service.notifyStallOnDenial(o.row.ID, o.row.LogicalAgentID)
			}
		}
	}
	opts.TypedEventCallback = gop.EventsCallback(func(ev gopevents.Event) {
		if existing != nil {
			existing(ev)
		}
		if permission != nil {
			permission(ev)
		}
		o.service.turnFeeds[o.runtimeID].observe(o, ev)
	})
}

func (o *sessionTurnOutput) publish(result turnoutput.Output) {
	if result.Kind == turnoutput.KindFinal && result.Text == "" {
		return
	}
	if o.service.Bus == nil {
		return
	}
	job := &turnOutputWrite{origin: o.runtimeOrigins[result.TurnID], storage: o.service.turnOutputStore, row: o.row, route: o.route, routeUnread: o.routeUnread, result: result, providerResultID: o.providerResultID, freshConversation: o.freshConversationTurn != "" && o.freshConversationTurn == o.turnID}
	// Claim under the retry lock while committing the journal, so a replay
	// sweep cannot race the synchronous publication of this same output.
	r := &o.service.outputRetries
	r.mu.Lock()
	journalErr := job.journal(o.service)
	if errors.Is(journalErr, store.ErrDuplicateWithheldOutput) {
		r.mu.Unlock()
		log.Printf("ERROR session %q turn %q: duplicate withheld output refused", o.row.ID, result.TurnID)
		return
	}
	if r.active == nil {
		r.active = make(map[string]bool)
	}
	r.active[job.journalID] = true
	r.mu.Unlock()
	ctx, cancel := o.service.outputPersistenceContext()
	defer cancel()
	persistErr := journalErr
	if persistErr == nil {
		persistErr = job.persist(ctx, o.service)
	}
	if err := persistErr; err != nil {
		log.Printf("ERROR session %q turn %q: output persistence deferred: %v", o.row.ID, result.TurnID, err)
		o.route, o.routeUnread = job.route, job.routeUnread
		o.service.retryTurnOutput(job)
		return
	}
	o.route, o.routeUnread = job.route, job.routeUnread
	r.mu.Lock()
	delete(r.active, job.journalID)
	r.mu.Unlock()
}

type turnOutputStore interface {
	GetSessionContext(context.Context, string) (*store.SessionRow, error)
	SessionRoute(context.Context, string) (*launchprofile.Route, error)
	StageTurnOutput(context.Context, messaging.Envelope) (messaging.Envelope, error)
}

// A successful stage is retained across event retries: publishing must never
// create a second durable body for the same output. The reader supplies one
// overall deadline; retry workers additionally bound each operation.
type turnOutputWrite struct {
	invariantFailed    bool
	rewriteJournalFrom string
	withheldReason     string
	withheldSize       int
	origin             *outputEventOrigin
	journalID          string
	journaled          bool
	journalClaimed     bool
	textTruncated      bool
	freshConversation  bool
	providerResultID   string
	storage            turnOutputStore
	row                store.SessionRow
	route              *launchprofile.Route
	routeUnread        bool
	result             turnoutput.Output
	messageID          string
}

func (w *turnOutputWrite) persist(parent context.Context, s *Service) error {
	if err := w.journal(s); err != nil {
		return err
	}
	ctx, cancel := s.outputPersistenceContextFrom(parent)
	published, found, err := s.Store.PublishedTurnOutput(ctx, w.row.ID, w.journalID)
	cancel()
	if err != nil {
		return err
	}
	if found {
		w.messageID = published.MessageID
		return w.completeJournal(s)
	}
	storage := w.storage
	if storage == nil {
		storage = s.Store
	}
	workstream, taskID := "", ""
	ctx, cancel = s.outputPersistenceContextFrom(parent)
	current, err := storage.GetSessionContext(ctx, w.row.ID)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil {
		log.Printf("ERROR session %q: read output workstream: %v", w.row.ID, err)
	} else {
		workstream = current.WorkstreamID.String
		taskID = current.TaskID.String
	}
	if w.routeUnread {
		ctx, cancel = s.outputPersistenceContextFrom(parent)
		route, routeErr := storage.SessionRoute(ctx, w.row.ID)
		cancel()
		if routeErr != nil {
			// An unavailable route is not an unrouted session. Keep its full
			// journal body until policy can be resolved, including DB errors.
			return routeErr
		}
		w.route, w.routeUnread = route, false
	}
	result := w.result
	payload := events.TurnOutputEvent{SessionID: w.row.ID, TurnID: result.TurnID, Kind: result.Kind,
		ProviderResultID: w.providerResultID, OutputID: w.journalID, FreshConversation: w.freshConversation,
		StopReason: result.StopReason, Confidence: result.Confidence, Runtime: result.Runtime,
		LogicalAgentID: w.row.LogicalAgentID, ProjectID: w.row.ProjectID, TaskID: taskID, WorkstreamID: workstream, MessageID: w.messageID,
		Reason: w.withheldReason, Size: w.withheldSize}
	if payload.MessageID == "" && w.route != nil && slices.Contains(w.route.Kinds, string(result.Kind)) {
		text, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{result.Text})
		ctx, cancel = s.outputPersistenceContextFrom(parent)
		env, stageErr := storage.StageTurnOutput(ctx, messaging.Envelope{
			From:     messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: w.row.ID},
			ThreadID: w.row.ID, Payload: text, ContentType: "application/json",
			Metadata: map[string]string{"session_id": w.row.ID, "turn_id": result.TurnID, "kind": string(result.Kind),
				"stop_reason": result.StopReason, "confidence": string(result.Confidence), "runtime": result.Runtime,
				"logical_agent_id": w.row.LogicalAgentID, "project_id": w.row.ProjectID, "workstream_id": workstream, "task_id": taskID,
				"output_id": w.journalID, "fresh_conversation": fmt.Sprint(w.freshConversation), "provider_result_id": w.providerResultID},
		})
		cancel()
		if stageErr != nil {
			return stageErr
		}
		w.messageID = env.ID
		payload.MessageID = env.ID
	}
	if payload.MessageID == "" && w.withheldReason == "" {
		payload.Text, payload.TextTruncated = turnOutputExcerpt(result.Text)
		payload.TextTruncated = payload.TextTruncated || w.textTruncated
	}
	data, _ := json.Marshal(payload)
	ctx, cancel = s.outputPersistenceContextFrom(parent)
	defer cancel()
	event := w.origin.apply(events.Event{Scope: events.ScopeSession, SessionID: w.row.ID,
		LogicalAgentID: w.row.LogicalAgentID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(data)})
	// Derived native metadata cannot replace the session's launcher-owned facts.
	event.Envelope.Dims.TaskID = taskID
	event.Envelope.Dims.ProjectID = w.row.ProjectID
	event.Envelope.Dims.WorkstreamID = workstream
	if err := s.Bus.Publish(ctx, event); err != nil {
		return err
	}
	return w.completeJournal(s)
}

func turnOutputExcerpt(text string) (string, bool) { return events.TurnOutputExcerpt(text) }

func (s *Service) outputPersistenceContext() (context.Context, context.CancelFunc) {
	return s.outputPersistenceContextFrom(context.Background())
}

func (s *Service) outputPersistenceContextFrom(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := s.turnOutputTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(parent, timeout)
}

// Every bridge lifecycle, including reattachment, settles pending output and
// wakes queued replies after process exit.
func (s *Service) finalizeSessionOutput(ctx context.Context, id string, output *sessionTurnOutput) {
	go func() {
		_, _ = s.Manager.WaitSession(context.WithoutCancel(ctx), id)
		if s.shouldFlushSessionOutput(context.WithoutCancel(ctx), id) {
			output.flush()
		}
		s.turnOutputs.CompareAndDelete(id, output)
	}()
}

func (w *turnOutputWrite) completeJournal(s *Service) error {
	if w.withheldReason == "secret_withheld" {
		// Keep a new producer claim from slipping between its DB lookup and
		// journal link while the first output retires its journal file.
		s.withheldOutputMu.Lock()
		defer s.withheldOutputMu.Unlock()
	}
	return s.Store.CompleteTurnOutputRetry(w.journalID)
}
