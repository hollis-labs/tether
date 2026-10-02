package app

import (
	"context"
	"encoding/json"
	"log"
	"slices"
	"sync"
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
	submissionGate   sync.Mutex
	accepted         bool
	completed        map[string]string
	completedDetails map[string]TurnCompletion
	completedOrder   []string
	mu               sync.Mutex
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
	route, err := s.Store.SessionRoute(context.Background(), row.ID)
	if err != nil {
		log.Printf("session %q: read output route: %v", row.ID, err)
	}
	out := &sessionTurnOutput{service: s, row: row, route: route}
	out.reducer = turnoutput.New(turnoutput.Config{SessionID: row.ID, Runtime: config.CanonicalRuntimeID(plan.ProviderBrand), NewTurnID: func() string {
		out.ensureTurn()
		out.reducerTurnID = out.turnID
		out.accepted = true
		return out.turnID
	}})
	return out
}

func (o *sessionTurnOutput) observeProvider(ev gopevents.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if result, ok := o.reducer.ObserveProvider(ev); ok {
		o.publish(result)
		o.completeTurn(result, false)
	}
}

func (o *sessionTurnOutput) observeRuntime(ev runtimeevents.Event) {
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
		if ev.TurnID != "" {
			o.bindTurn(ev.TurnID)
		}
	default:
		// Process/session telemetry does not open a turn.
	}
	if result, ok := o.reducer.Observe(ev); ok {
		o.publish(result)
		o.completeTurn(result, false)
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
	permission := makeProviderTypedEventCallback(o.service.Bus, o.row.ID, o.row.LogicalAgentID)
	opts.TypedEventCallback = gop.EventsCallback(func(ev gopevents.Event) {
		if existing != nil {
			existing(ev)
		}
		permission(ev)
		o.observeProvider(ev)
	})
}

func (o *sessionTurnOutput) publish(result turnoutput.Output) {
	if result.Kind == turnoutput.KindFinal && result.Text == "" {
		return
	}
	if o.service.Bus == nil {
		return
	}
	payload := events.TurnOutputEvent{SessionID: o.row.ID, TurnID: result.TurnID, Kind: result.Kind,
		StopReason: result.StopReason, Confidence: result.Confidence, Runtime: result.Runtime,
		LogicalAgentID: o.row.LogicalAgentID, ProjectID: o.row.ProjectID, WorkstreamID: o.row.WorkstreamID.String}
	if o.route != nil && slices.Contains(o.route.Kinds, string(result.Kind)) {
		text, _ := json.Marshal(struct {
			Text string `json:"text"`
		}{result.Text})
		env, err := o.service.Store.StageTurnOutput(context.Background(), messaging.Envelope{
			From:     messaging.Address{Kind: messaging.KindSession, Authority: "local", ID: o.row.ID},
			ThreadID: o.row.ID, Payload: text, ContentType: "application/json",
			Metadata: map[string]string{"session_id": o.row.ID, "turn_id": result.TurnID, "kind": string(result.Kind),
				"stop_reason": result.StopReason, "confidence": string(result.Confidence), "runtime": result.Runtime,
				"logical_agent_id": o.row.LogicalAgentID, "project_id": o.row.ProjectID, "workstream_id": o.row.WorkstreamID.String},
		})
		if err != nil {
			log.Printf("session %q turn %q: persist output: %v", o.row.ID, result.TurnID, err)
		} else {
			payload.MessageID = env.ID
		}
	}
	if payload.MessageID == "" {
		payload.Text, payload.TextTruncated = turnOutputExcerpt(result.Text)
	}
	data, _ := json.Marshal(payload)
	if err := o.service.Bus.Publish(context.Background(), events.Event{Scope: events.ScopeSession, SessionID: o.row.ID,
		LogicalAgentID: o.row.LogicalAgentID, Kind: events.KindSessionTurnOutput, PayloadJSON: string(data)}); err != nil {
		log.Printf("session %q turn %q: publish output: %v", o.row.ID, result.TurnID, err)
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
