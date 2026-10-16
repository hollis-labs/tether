package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// X1c (CW-20261016-0136): the events table persists the v1.1 envelope blocks
// (migration 0082). Writers fill what they can verify; everything else is NULL.

var _ events.EnvelopePersister = (*Store)(nil)

// envelopeColumns are the metadata columns from migrations 0082 and 0086, in the order
// envelopeValues returns their values.
var envelopeColumns = []string{
	"scope_environment_id", "scope_authority", "scope_worker_id", "scope_host", "scope_project_id",
	"scope_workstream_id", "scope_epic_id", "scope_sprint_id", "scope_plan_id", "scope_phase_id",
	"scope_task_id", "scope_run_id", "scope_attempt", "scope_launch_id", "scope_agent_urn",
	"scope_logical_agent_id", "scope_team_run_id",
	"category", "severity", "sensitivity", "pii", "visibility", "payload_schema", "sanitized", "redactions", "labels_json",
	"content_type", "truncated",
	"trace_id", "span_id", "causation_id", "correlation_id", "thread_id", "idempotency_key", "depth",
	"actor_kind", "actor_urn", "actor_verified", "on_behalf_of",
	"recorded_at", "epoch",
	"retention_class", "retention_policy_id",
	"schema_version",
	"producer_app",
	"subject",
	"turn_id",
	"parent_id",
	"raw_offset",
	"process_pid",
	"process_provider",
	"process_runtime",
	"provider_session_id",
	"source_channel",
	"source_confidence",
	"producer_sequence",
	"producer_generation",
	"traceparent",
	"in_reply_to",
	"actor_role",
	"on_behalf_of_json",
}

func nullIfZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullUint64(n uint64) any {
	if n == 0 {
		return nil
	}
	return strconv.FormatUint(n, 10)
}

// flag stores a boolean as 1, or NULL when false: NULL means "not marked".
func flag(b bool) any {
	if b {
		return 1
	}
	return nil
}

// envelopeValues returns one value per envelopeColumns entry. Empty strings and
// zero numbers are stored as NULL. recordedAt is the log's own stamp.
func envelopeValues(e events.Event, recordedAt string) ([]any, error) {
	env := e.Envelope
	if err := env.Validate(); err != nil {
		return nil, err
	}
	d := env.Dims
	logical := d.LogicalAgentID
	if logical == "" {
		logical = e.LogicalAgentID
	}
	var labels any
	if len(env.Class.Labels) > 0 {
		raw, err := json.Marshal(env.Class.Labels)
		if err != nil {
			return nil, err
		}
		labels = string(raw)
	}
	var delegation any
	if len(env.Actor.OnBehalfOfChain) > 0 {
		raw, err := json.Marshal(env.Actor.OnBehalfOfChain)
		if err != nil {
			return nil, err
		}
		delegation = string(raw)
	}
	return []any{
		nullIfEmpty(d.EnvironmentID), nullIfEmpty(d.Authority), nullIfEmpty(d.WorkerID), nullIfEmpty(d.Host), nullIfEmpty(d.ProjectID),
		nullIfEmpty(d.WorkstreamID), nullIfEmpty(d.EpicID), nullIfEmpty(d.SprintID), nullIfEmpty(d.PlanID), nullIfEmpty(d.PhaseID),
		nullIfEmpty(d.TaskID), nullIfEmpty(d.RunID), nullIfZero(d.Attempt), nullIfEmpty(d.LaunchID), nullIfEmpty(d.AgentURN),
		nullIfEmpty(logical), nullIfEmpty(d.TeamRunID),
		nullIfEmpty(env.Class.Category), nullIfEmpty(env.Class.Severity), nullIfEmpty(env.Class.Sensitivity), flag(env.Class.PII),
		nullIfEmpty(env.Class.Visibility), nullIfEmpty(env.Class.PayloadSchema), flag(env.Class.Sanitized), nullIfZero(env.Class.Redactions), labels,
		nullIfEmpty(env.ContentType), flag(env.Truncated),
		nullIfEmpty(env.Corr.TraceID), nullIfEmpty(env.Corr.SpanID), nullIfEmpty(env.Corr.CausationID), nullIfEmpty(env.Corr.CorrelationID),
		nullIfEmpty(env.Corr.ThreadID), nullIfEmpty(env.Corr.IdempotencyKey), nullIfZero(env.Corr.Depth),
		nullIfEmpty(env.Actor.Kind), nullIfEmpty(env.Actor.URN), nullIfEmpty(env.Actor.Verified), nullIfEmpty(env.Actor.OnBehalfOf),
		recordedAt, nil,
		nullIfEmpty(env.RetentionClass), nullIfEmpty(env.RetentionPolicyID),
		nullIfEmpty(env.SchemaVersion),
		nullIfEmpty(env.App),
		nullIfEmpty(env.Subject),
		nullIfEmpty(env.TurnID),
		nullIfEmpty(env.ParentID),
		env.RawOffset,
		nullIfZero(env.Process.PID),
		nullIfEmpty(env.Process.Provider),
		nullIfEmpty(env.Process.Runtime),
		nullIfEmpty(env.Process.ProviderSessionID),
		nullIfEmpty(env.Source.Channel),
		nullIfEmpty(env.Source.Confidence),
		nullUint64(env.Sequence),
		nullUint64(env.Generation),
		nullIfEmpty(env.Corr.Traceparent),
		nullIfEmpty(env.Corr.InReplyTo),
		nullIfEmpty(env.Actor.Role),
		delegation,
	}, nil
}

// insertEventSQL is the one INSERT every events write goes through.
var insertEventSQL = `INSERT INTO events (scope, session_id, at, kind, payload_json, event_id, pending, ` +
	strings.Join(envelopeColumns, ", ") + `) VALUES (?, ?, ?, ?, ?, ?, ?, ` +
	strings.TrimSuffix(strings.Repeat("?, ", len(envelopeColumns)), ", ") + `)
 ON CONFLICT(event_id) WHERE event_id IS NOT NULL DO NOTHING`

// InsertEventEnvelope implements events.EnvelopePersister: it writes e with its
// envelope blocks, a D6 id and a log-assigned recorded_at, and returns the stored
// event. The environment-request kind keeps its own bookkeeping path.
func (s *Store) InsertEventEnvelope(ctx context.Context, e events.Event) (events.Event, error) {
	if e.Scope == "" {
		return events.Event{}, fmt.Errorf("event scope required")
	}
	if e.Kind == EnvironmentRequestKind {
		return s.insertEnvironmentRequestEnvelope(ctx, e)
	}
	at := time.Now().UTC()
	stored, _, err := insertEventEnvelopeRow(ctx, s.db, e, at, "", false)
	return stored, err
}

// insertEventEnvelopeRow writes e on w (a transaction or the pool). pending marks
// an outbox row. A repeat of an event id already stored writes nothing and
// reports existing=true with the stored sequence.
func insertEventEnvelopeRow(ctx context.Context, w eventWriter, e events.Event, at time.Time, eventID string, pending bool) (events.Event, bool, error) {
	if err := applyEventRetentionAdmission(ctx, w, &e); err != nil {
		return events.Event{}, false, err
	}
	if eventID == "" {
		eventID = e.EventID
	}
	if eventID == "" {
		eventID = newEventID()
	}
	if !e.At.IsZero() {
		at = e.At
	}
	stamp := at.UTC().Format(time.RFC3339Nano)
	recorded := time.Now().UTC().Format(time.RFC3339Nano)
	values, err := envelopeValues(e, recorded)
	if err != nil {
		return events.Event{}, false, err
	}
	pendingFlag := 0
	if pending {
		pendingFlag = 1
	}
	args := append([]any{e.Scope, nullIfEmpty(e.SessionID), stamp, e.Kind, nullIfEmpty(e.PayloadJSON), eventID, pendingFlag}, values...)
	res, err := w.ExecContext(ctx, insertEventSQL, args...)
	if err != nil {
		return events.Event{}, false, err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		stored, err := scanEvent(w.QueryRowContext(ctx, `SELECT `+eventReadColumns+` FROM events WHERE event_id = ?`, eventID))
		if err != nil {
			return events.Event{}, false, fmt.Errorf("event %s already stored but unreadable: %w", eventID, err)
		}
		return stored, true, nil
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return events.Event{}, false, err
	}
	e.Seq, e.At, e.EventID = seq, at.UTC(), eventID
	e.Envelope.Epoch = 0
	e.Envelope.PayloadDigest = ""
	if e.Envelope.Dims.LogicalAgentID == "" {
		e.Envelope.Dims.LogicalAgentID = e.LogicalAgentID
	}
	e.LogicalAgentID = e.Envelope.Dims.LogicalAgentID
	if t, perr := time.Parse(time.RFC3339Nano, recorded); perr == nil {
		e.Envelope.RecordedAt = t
	}
	return e, false, nil
}
