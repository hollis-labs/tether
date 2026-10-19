package environmentstream

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents"
	mesh "github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/store"
)

var ErrBackfillInProgress = errors.New("backfill_in_progress")

// StoredIDProjection uses the frozen identity/cutover seeded before producers.
// New leaves it disabled; NewStored checks readiness before serving it.
// Live deployment requires the owner's release-N mint proof (CW-20261016-0136).
type StoredIDProjection struct {
	Cutover int64
}

func (p StoredIDProjection) checkID(e events.Event) error {
	if p.Cutover < 0 || e.Seq < 1 {
		return fmt.Errorf("invalid frozen event coordinates")
	}
	if e.EventID != "" {
		return nil
	}
	if e.Seq <= p.Cutover {
		return ErrBackfillInProgress
	}
	return store.ErrEventIDsAfterCutover
}

// MapStored projects the persisted producer envelope. IDs are opaque strings:
// both frozen legacy <environment>:<seq> IDs and minted evt_ IDs pass through.
// The durable cursor is always the log sequence, never the producer sequence.
func (p StoredIDProjection) MapStored(environmentID string, e events.Event) (Event, error) {
	if err := p.checkID(e); err != nil {
		return Event{}, err
	}
	logSequence := e.Seq
	if environmentID == "" || logSequence < 1 || strings.ContainsAny(e.Kind, "\r\n") {
		return Event{}, fmt.Errorf("missing durable event identity")
	}
	v := e.Envelope
	if err := v.Validate(); err != nil {
		return Event{}, err
	}
	attempt, err := wireCounter(v.Dims.Attempt)
	if err != nil {
		return Event{}, err
	}
	depth, err := wireCounter(v.Corr.Depth)
	if err != nil {
		return Event{}, err
	}
	redactions, err := wireCounter(v.Class.Redactions)
	if err != nil {
		return Event{}, err
	}
	r := runtimeevents.Event{
		SchemaVersion: v.SchemaVersion, ID: e.EventID, Kind: runtimeevents.EventKind(e.Kind), Time: e.At,
		App: v.App, SessionID: e.SessionID, TurnID: v.TurnID, ParentID: v.ParentID, Subject: v.Subject,
		Sequence: v.Sequence, Generation: v.Generation, Cursor: strconv.FormatInt(e.Seq, 10),
		ContentType: v.ContentType, Truncated: v.Truncated, Payload: json.RawMessage(e.PayloadJSON),
		Process: runtimeevents.Process{PID: v.Process.PID, Provider: v.Process.Provider, Runtime: v.Process.Runtime, ProviderSessionID: v.Process.ProviderSessionID},
		Source:  runtimeevents.Source{Channel: runtimeevents.SourceChannel(v.Source.Channel), Confidence: runtimeevents.Confidence(v.Source.Confidence)},
		Scope: &runtimeevents.Scope{
			EnvironmentID: v.Dims.EnvironmentID, Authority: v.Dims.Authority, WorkerID: v.Dims.WorkerID, Host: v.Dims.Host,
			ProjectID: v.Dims.ProjectID, WorkstreamID: v.Dims.WorkstreamID, EpicID: v.Dims.EpicID, SprintID: v.Dims.SprintID,
			PlanID: v.Dims.PlanID, PhaseID: v.Dims.PhaseID, TaskID: v.Dims.TaskID, RunID: v.Dims.RunID, Attempt: attempt,
			LaunchID: v.Dims.LaunchID, AgentURN: v.Dims.AgentURN, LogicalAgentID: v.Dims.LogicalAgentID, TeamRunID: v.Dims.TeamRunID,
		},
		Corr: &runtimeevents.Corr{
			TraceID: v.Corr.TraceID, SpanID: v.Corr.SpanID, Traceparent: v.Corr.Traceparent, InReplyTo: v.Corr.InReplyTo,
			CausationID: v.Corr.CausationID, CorrelationID: v.Corr.CorrelationID, ThreadID: v.Corr.ThreadID,
			IdempotencyKey: v.Corr.IdempotencyKey, Depth: depth,
		},
		Class: &runtimeevents.Class{
			Category: v.Class.Category, Severity: v.Class.Severity, Sensitivity: v.Class.Sensitivity, PII: v.Class.PII,
			Visibility: v.Class.Visibility, PayloadSchema: v.Class.PayloadSchema, Sanitized: v.Class.Sanitized,
			Redactions: redactions, Labels: slices.Clone(v.Class.Labels),
		},
	}
	if v.RawOffset != nil {
		offset := *v.RawOffset
		r.RawOffset = &offset
	}
	if !v.RecordedAt.IsZero() {
		recorded := v.RecordedAt
		r.RecordedAt = &recorded
	}
	if r.SchemaVersion == "" {
		r.SchemaVersion = runtimeevents.SchemaVersion
	}
	if r.App == "" {
		r.App = "tether"
	}
	if r.Subject == "" {
		r.Subject = "urn:tether:environment:" + environmentID
		if e.SessionID != "" {
			r.Subject = "urn:session:" + e.SessionID
		}
	}
	if r.Scope.EnvironmentID == "" {
		r.Scope.EnvironmentID = environmentID
	}
	if r.Scope.LogicalAgentID == "" {
		r.Scope.LogicalAgentID = e.LogicalAgentID
	}
	if r.ContentType == "" {
		r.ContentType = "application/json"
	}
	if r.Class.Visibility == "" {
		r.Class.Visibility = "local"
	}
	if len(r.Payload) == 0 {
		r.Payload = json.RawMessage(`null`)
	}
	// Legacy rows have no producer coordinates. Report the durable log as the
	// source only when both are absent; never attach its cursor to a known
	// producer channel or pretend an incomplete producer coordinate is valid.
	if r.Sequence == 0 && r.Source.Channel == "" {
		r.Sequence = uint64(logSequence)
		r.Source = runtimeevents.Source{Channel: "tether.events", Confidence: runtimeevents.ConfidenceExact}
	} else if r.Sequence == 0 || r.Source.Channel == "" {
		return Event{}, fmt.Errorf("incomplete producer event coordinates")
	}
	if r.Generation == 0 {
		r.Generation = 1 // runtimeevents contract: zero/unknown is read as one.
	}
	actor := mesh.Actor{URN: mesh.URN(v.Actor.URN), Kind: mesh.ActorKind(v.Actor.Kind)}
	if actor.Validate() == nil {
		chain := slices.Clone(v.Actor.OnBehalfOfChain)
		if len(chain) == 0 && v.Actor.OnBehalfOf != "" {
			chain = []string{v.Actor.OnBehalfOf}
		}
		r.Actor = &runtimeevents.Actor{URN: v.Actor.URN, Kind: v.Actor.Kind, Role: v.Actor.Role, Verified: v.Actor.Verified, OnBehalfOf: chain}
	} else {
		// The hub reports the row, it does not become its verified author.
		r.Actor = &runtimeevents.Actor{URN: "msg://service/tether/" + environmentID, Kind: string(mesh.ActorService), Role: "system", Verified: "none"}
	}
	mapped, err := runtimeevents.ToMesh(r)
	if err != nil {
		return Event{}, err
	}
	mapped.PayloadDigest = v.PayloadDigest
	out := Event{EnvironmentID: environmentID, Seq: e.Seq, Event: mapped}
	return out, out.Validate()
}

func wireCounter(n int) (uint32, error) {
	value := int64(n)
	if value < 0 || value > math.MaxUint32 {
		return 0, fmt.Errorf("event envelope counter exceeds wire range")
	}
	return uint32(value), nil
}
