package app

import (
	"context"
	"encoding/json"
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
	submissionGate sync.Mutex
	accepted       bool
	submissions    int
	routeUnread    bool
	completed      map[string]string
	completedOrder []string
	mu             sync.Mutex
	runtimeID      string
	reducer        *turnoutput.Reducer
	service        *Service
	row            store.SessionRow
	route          *launchprofile.Route
	turnID         string
	reducerTurnID  string
	turnDone       chan struct{}
	finishedTurns  []string
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
		o.completeTurn(result.TurnID)
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
		o.completeTurn(result.TurnID)
	}
}

func (o *sessionTurnOutput) flush() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if result, ok := o.reducer.Flush("process_exited"); ok {
		o.publish(result)
		o.completeTurn(result.TurnID)
	}
	// A process can end before its first reduced event.
	o.settleTurn()
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
	ctx, cancel := o.service.outputPersistenceContext()
	defer cancel()
	workstream := ""
	current, err := o.service.Store.GetSessionContext(ctx, o.row.ID)
	if err != nil {
		log.Printf("ERROR session %q: read current output workstream: %v", o.row.ID, err)
	} else {
		workstream = current.WorkstreamID.String
	}
	if o.routeUnread {
		route, err := o.service.Store.SessionRoute(ctx, o.row.ID)
		if err != nil {
			log.Printf("ERROR session %q: reread output route; full-text routing unavailable: %v", o.row.ID, err)
		} else {
			o.route = route
			o.routeUnread = false
		}
	}
	payload := events.TurnOutputEvent{SessionID: o.row.ID, TurnID: result.TurnID, Kind: result.Kind,
		StopReason: result.StopReason, Confidence: result.Confidence, Runtime: result.Runtime,
		LogicalAgentID: o.row.LogicalAgentID, ProjectID: o.row.ProjectID, WorkstreamID: workstream}
	if o.route != nil && slices.Contains(o.route.Kinds, string(result.Kind)) {
		text, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{result.Text})
		env, err := o.service.Store.StageTurnOutput(ctx, messaging.Envelope{
			From:     messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: o.row.ID},
			ThreadID: o.row.ID, Payload: text, ContentType: "application/json",
			Metadata: map[string]string{"session_id": o.row.ID, "turn_id": result.TurnID, "kind": string(result.Kind),
				"stop_reason": result.StopReason, "confidence": string(result.Confidence), "runtime": result.Runtime,
				"logical_agent_id": o.row.LogicalAgentID, "project_id": o.row.ProjectID, "workstream_id": workstream},
		})
		if err != nil {
			log.Printf("ERROR session %q turn %q: full-text output persistence failed: %v", o.row.ID, result.TurnID, err)
		} else {
			payload.MessageID = env.ID
		}
	}
	if payload.MessageID == "" {
		payload.Text, payload.TextTruncated = turnOutputExcerpt(result.Text)
	}
	data, _ := json.Marshal(payload)
	if err := o.service.Bus.Publish(ctx, events.Event{Scope: events.ScopeSession, SessionID: o.row.ID,
		LogicalAgentID: o.row.LogicalAgentID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(data)}); err != nil {
		log.Printf("ERROR session %q turn %q: output event persistence failed: %v", o.row.ID, result.TurnID, err)
	}
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
	timeout := s.turnOutputTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return context.WithTimeout(context.Background(), timeout)
}
