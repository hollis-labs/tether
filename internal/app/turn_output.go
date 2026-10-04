package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	messaging "github.com/hollis-labs/go-messaging"
	gop "github.com/hollis-labs/go-providers/provider"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/go-runtime-events/runtimeevents"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/launchprofile"
	"github.com/hollis-labs/tether/internal/store"
)

// The outer lock keeps reduction, persistence and emission ordered across
// callback/termination races; the raw feeds are synchronous and lossless.
type sessionTurnOutput struct {
	providerResultID string
	submissionGate   sync.Mutex
	accepted         bool
	submissions      int
	activity         uint64
	unboundTerminal  *emptyTurnTerminal
	unboundAmbiguous bool
	routeUnread      bool
	completed        map[string]string
	completedDetails map[string]TurnCompletion
	completedOrder   []string
	mu               sync.Mutex
	runtimeID        string
	reducer          *turnoutput.Reducer
	service          *Service
	row              store.SessionRow
	route            *launchprofile.Route
	turnID           string
	reducerTurnID    string
	turnDone         chan struct{}
	finishedTurns    []string
}

func (s *Service) newSessionTurnOutput(row store.SessionRow, plan *launch.Plan) *sessionTurnOutput {
	ctx, cancel := s.outputPersistenceContext()
	defer cancel()
	route, err := s.Store.SessionRoute(ctx, row.ID)
	if err != nil {
		log.Printf("ERROR session %q: read output route; full-text routing unavailable: %v", row.ID, err)
	}
	out := &sessionTurnOutput{service: s, row: row, route: route, routeUnread: err != nil, runtimeID: config.CanonicalRuntimeID(plan.ProviderBrand)}
	out.reducer = turnoutput.New(turnoutput.Config{SessionID: row.ID, Runtime: config.CanonicalRuntimeID(plan.ProviderBrand), QuestionTools: s.questionTools(config.CanonicalRuntimeID(plan.ProviderBrand)), NewTurnID: func() string {
		out.ensureTurn()
		out.noteTurnActivity()
		out.reducerTurnID = out.turnID
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
		} else if registration.approval == nil {
			return
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if ev.TurnID != "" && slices.Contains(o.finishedTurns, ev.TurnID) {
		return
	}
	switch ev.Kind {
	case runtimeevents.KindTurnStarted, runtimeevents.KindAgentDelta, runtimeevents.KindAgentToolUse,
		runtimeevents.KindAgentToolResult, runtimeevents.KindAgentSubagentSpawn, runtimeevents.KindAgentPermissionRequested,
		runtimeevents.KindAgentPermissionResolved, runtimeevents.KindAgentPermissionDenied,
		runtimeevents.KindTurnCompleted, runtimeevents.KindTurnFailed:
		if ev.TurnID != "" && (o.reducerTurnID == "" || ev.Kind == runtimeevents.KindTurnStarted || ev.TurnID == o.reducerTurnID) {
			o.bindTurn(ev.TurnID)
		}
	default:
		// Process/session telemetry does not open a turn.
	}
	if result, ok := o.reducer.Observe(ev); ok {
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
	if o.runtimeID == "antigravity" {
		permission = makeProviderTypedEventCallback(o.service.Bus, o.row.ID, o.row.LogicalAgentID)
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
	job := &turnOutputWrite{storage: o.service.turnOutputStore, row: o.row, route: o.route, routeUnread: o.routeUnread, result: result, providerResultID: o.providerResultID}
	ctx, cancel := o.service.outputPersistenceContext()
	defer cancel()
	if err := job.persist(ctx, o.service); err != nil {
		log.Printf("ERROR session %q turn %q: output persistence deferred: %v", o.row.ID, result.TurnID, err)
		o.route, o.routeUnread = job.route, job.routeUnread
		o.service.retryTurnOutput(job)
		return
	}
	o.route, o.routeUnread = job.route, job.routeUnread
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
	providerResultID string
	storage          turnOutputStore
	row              store.SessionRow
	route            *launchprofile.Route
	routeUnread      bool
	result           turnoutput.Output
	messageID        string
}

func (w *turnOutputWrite) persist(parent context.Context, s *Service) error {
	storage := w.storage
	if storage == nil {
		storage = s.Store
	}
	workstream := ""
	ctx, cancel := s.outputPersistenceContextFrom(parent)
	current, err := storage.GetSessionContext(ctx, w.row.ID)
	cancel()
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return err
	}
	if err != nil {
		log.Printf("ERROR session %q: read output workstream: %v", w.row.ID, err)
	} else {
		workstream = current.WorkstreamID.String
	}
	if w.routeUnread {
		ctx, cancel = s.outputPersistenceContextFrom(parent)
		route, routeErr := storage.SessionRoute(ctx, w.row.ID)
		cancel()
		if errors.Is(routeErr, context.DeadlineExceeded) || errors.Is(routeErr, context.Canceled) {
			return routeErr
		}
		if routeErr != nil {
			log.Printf("ERROR session %q: reread output route; full-text routing unavailable: %v", w.row.ID, routeErr)
		} else {
			w.route, w.routeUnread = route, false
		}
	}
	result := w.result
	payload := events.TurnOutputEvent{SessionID: w.row.ID, TurnID: result.TurnID, Kind: result.Kind,
		ProviderResultID: w.providerResultID,
		StopReason:       result.StopReason, Confidence: result.Confidence, Runtime: result.Runtime,
		LogicalAgentID: w.row.LogicalAgentID, ProjectID: w.row.ProjectID, WorkstreamID: workstream, MessageID: w.messageID}
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
				"logical_agent_id": w.row.LogicalAgentID, "project_id": w.row.ProjectID, "workstream_id": workstream},
		})
		cancel()
		if stageErr != nil {
			if errors.Is(stageErr, context.DeadlineExceeded) || errors.Is(stageErr, context.Canceled) {
				return stageErr
			}
			log.Printf("ERROR session %q turn %q: full-text output persistence failed: %v", w.row.ID, result.TurnID, stageErr)
		} else {
			w.messageID = env.ID
			payload.MessageID = env.ID
		}
	}
	if payload.MessageID == "" {
		payload.Text, payload.TextTruncated = turnOutputExcerpt(result.Text)
	}
	data, _ := json.Marshal(payload)
	ctx, cancel = s.outputPersistenceContextFrom(parent)
	defer cancel()
	return s.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: w.row.ID,
		LogicalAgentID: w.row.LogicalAgentID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(data)})
}

func turnOutputExcerpt(text string) (string, bool) {
	const maxBytes = 4 * 1024
	if len(text) <= maxBytes {
		return text, false
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end], true
}

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
		output.flush()
		s.turnOutputs.CompareAndDelete(id, output)
	}()
}
