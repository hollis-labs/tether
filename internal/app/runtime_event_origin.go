package app

import (
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	"github.com/hollis-labs/tether/internal/events"
)

// outputEventOrigin contains envelope metadata only. Never retain the native
// payload here: this object can enter the durable output retry journal.
type outputEventOrigin struct {
	At       time.Time       `json:"at,omitempty"`
	Envelope events.Envelope `json:"envelope"`
}

// derivedRuntimeOrigin preserves provenance for approved derived payloads.
// The native ID is causal, not structural: ParentID retains its subagent link.
// Fresh derived IDs and log coordinates are assigned by the normal bus/store.
func derivedRuntimeOrigin(ev runtimeevents.Event) (*outputEventOrigin, error) {
	v := events.Envelope{
		SchemaVersion: ev.SchemaVersion, App: ev.App, Subject: ev.Subject, TurnID: ev.TurnID, ParentID: ev.ParentID,
		Process:  events.Process{PID: ev.Process.PID, Provider: ev.Process.Provider, Runtime: ev.Process.Runtime, ProviderSessionID: ev.Process.ProviderSessionID},
		Source:   events.Source{Channel: string(ev.Source.Channel), Confidence: string(ev.Source.Confidence)},
		Sequence: ev.Sequence, Generation: ev.Generation, ContentType: "application/json", Truncated: ev.Truncated,
	}
	if ev.RawOffset != nil {
		offset := *ev.RawOffset
		v.RawOffset = &offset
	}
	if d := ev.Scope; d != nil {
		v.Dims = events.ScopeDims{EnvironmentID: d.EnvironmentID, Authority: d.Authority, WorkerID: d.WorkerID, Host: d.Host,
			ProjectID: d.ProjectID, WorkstreamID: d.WorkstreamID, EpicID: d.EpicID, SprintID: d.SprintID, PlanID: d.PlanID,
			PhaseID: d.PhaseID, TaskID: d.TaskID, RunID: d.RunID, Attempt: int(d.Attempt), LaunchID: d.LaunchID,
			AgentURN: d.AgentURN, LogicalAgentID: d.LogicalAgentID, TeamRunID: d.TeamRunID}
	}
	if c := ev.Corr; c != nil {
		v.Corr = events.Corr{TraceID: c.TraceID, SpanID: c.SpanID, Traceparent: c.Traceparent, CausationID: c.CausationID,
			CorrelationID: c.CorrelationID, ThreadID: c.ThreadID, InReplyTo: c.InReplyTo, IdempotencyKey: c.IdempotencyKey, Depth: int(c.Depth)}
	}
	if ev.ID != "" {
		if ev.Corr != nil && ev.Corr.Depth == math.MaxUint32 {
			return nil, fmt.Errorf("native event causation depth cannot be incremented")
		}
		v.Corr.CausationID = ev.ID
		v.Corr.Depth++
	}
	if a := ev.Actor; a != nil {
		v.Actor = events.Actor{Kind: a.Kind, URN: a.URN, Role: a.Role, Verified: a.Verified, OnBehalfOfChain: slices.Clone(a.OnBehalfOf)}
	}
	if c := ev.Class; c != nil {
		// The kind/payload changed. The trusted X4 stage must classify and
		// sanitize the new payload; native sanitation is not transferable.
		// Sensitivity and PII remain floors, never downgraded by derivation.
		v.Class = events.Class{Category: c.Category, Severity: c.Severity, Sensitivity: c.Sensitivity, PII: c.PII,
			Visibility: c.Visibility, Labels: slices.Clone(c.Labels)}
	}
	return &outputEventOrigin{At: ev.Time, Envelope: v}, nil
}

func (origin *outputEventOrigin) apply(e events.Event) events.Event {
	if origin != nil {
		e.At = origin.At
		e.Envelope = origin.Envelope
	}
	return e
}

// Output reduces several native inputs. A later terminal marker with no class
// must not erase a sensitivity/PII floor observed on an earlier contributing
// delta. settleTurn clears the accumulator before another turn starts.
func combineOutputOrigin(previous, next *outputEventOrigin) *outputEventOrigin {
	if previous != nil && next != nil {
		next.Envelope.Class.PII = next.Envelope.Class.PII || previous.Envelope.Class.PII
		if previous.Envelope.Class.Sensitivity == "secret" || (next.Envelope.Class.Sensitivity != "secret" && sensitivityRank(previous.Envelope.Class.Sensitivity) > sensitivityRank(next.Envelope.Class.Sensitivity)) {
			next.Envelope.Class.Sensitivity = previous.Envelope.Class.Sensitivity
		}
		next.Envelope.Truncated = next.Envelope.Truncated || previous.Envelope.Truncated
	}
	return next
}

func sensitivityRank(value string) int {
	switch value {
	case "":
		return 0
	case "public":
		return 1
	case "private":
		return 2
	case "secret":
		return 3
	default:
		// Preserve an unrecognized floor for admission to reject/hold; never
		// silently replace it with a recognized, potentially weaker value.
		return 4
	}
}
