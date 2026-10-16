package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/events"
)

// eventReadColumns is shared by replay, outbox, environment and session reads.
// Nullable metadata decodes to its unknown zero value, never inferred values.
const eventReadColumns = `COALESCE(id,0),
 COALESCE(scope,''),
 COALESCE(session_id,''),
 COALESCE(at,''),
 COALESCE(kind,''),
 COALESCE(payload_json,''),
 COALESCE(event_id,''),
 COALESCE(scope_environment_id,''),
 COALESCE(scope_authority,''),
 COALESCE(scope_worker_id,''),
 COALESCE(scope_host,''),
 COALESCE(scope_project_id,''),
 COALESCE(scope_workstream_id,''),
 COALESCE(scope_epic_id,''),
 COALESCE(scope_sprint_id,''),
 COALESCE(scope_plan_id,''),
 COALESCE(scope_phase_id,''),
 COALESCE(scope_task_id,''),
 COALESCE(scope_run_id,''),
 COALESCE(scope_attempt,0),
 COALESCE(scope_launch_id,''),
 COALESCE(scope_agent_urn,''),
 COALESCE(scope_logical_agent_id,''),
 COALESCE(scope_team_run_id,''),
 COALESCE(category,''),
 COALESCE(severity,''),
 COALESCE(sensitivity,''),
 COALESCE(pii,0),
 COALESCE(visibility,''),
 COALESCE(payload_schema,''),
 COALESCE(sanitized,0),
 COALESCE(redactions,0),
 COALESCE(labels_json,''),
 COALESCE(content_type,''),
 COALESCE(truncated,0),
 COALESCE(trace_id,''),
 COALESCE(span_id,''),
 COALESCE(causation_id,''),
 COALESCE(correlation_id,''),
 COALESCE(thread_id,''),
 COALESCE(idempotency_key,''),
 COALESCE(depth,0),
 COALESCE(actor_kind,''),
 COALESCE(actor_urn,''),
 COALESCE(actor_verified,''),
 COALESCE(on_behalf_of,''),
 COALESCE(recorded_at,''),
 COALESCE(epoch,0),
 COALESCE(retention_class,''),
 COALESCE(retention_policy_id,''),
 COALESCE(schema_version,''),
 COALESCE(producer_app,''),
 COALESCE(subject,''),
 COALESCE(turn_id,''),
 COALESCE(parent_id,''),
 raw_offset,
 COALESCE(process_pid,0),
 COALESCE(process_provider,''),
 COALESCE(process_runtime,''),
 COALESCE(provider_session_id,''),
 COALESCE(source_channel,''),
 COALESCE(source_confidence,''),
 COALESCE(producer_sequence,0),
 COALESCE(producer_generation,0),
 COALESCE(traceparent,''),
 COALESCE(in_reply_to,''),
 COALESCE(actor_role,''),
 COALESCE(on_behalf_of_json,''),
 COALESCE(origin_at,''),
 COALESCE(payload_digest,'')`

type eventScanner interface{ Scan(...any) error }

func scanEvent(row eventScanner) (events.Event, error) {
	var e events.Event
	env := &e.Envelope
	var at, recorded, origin, labels, delegation string
	var rawOffset sql.NullInt64
	if err := row.Scan(
		&e.Seq,
		&e.Scope,
		&e.SessionID,
		&at,
		&e.Kind,
		&e.PayloadJSON,
		&e.EventID,
		&env.Dims.EnvironmentID,
		&env.Dims.Authority,
		&env.Dims.WorkerID,
		&env.Dims.Host,
		&env.Dims.ProjectID,
		&env.Dims.WorkstreamID,
		&env.Dims.EpicID,
		&env.Dims.SprintID,
		&env.Dims.PlanID,
		&env.Dims.PhaseID,
		&env.Dims.TaskID,
		&env.Dims.RunID,
		&env.Dims.Attempt,
		&env.Dims.LaunchID,
		&env.Dims.AgentURN,
		&env.Dims.LogicalAgentID,
		&env.Dims.TeamRunID,
		&env.Class.Category,
		&env.Class.Severity,
		&env.Class.Sensitivity,
		&env.Class.PII,
		&env.Class.Visibility,
		&env.Class.PayloadSchema,
		&env.Class.Sanitized,
		&env.Class.Redactions,
		&labels,
		&env.ContentType,
		&env.Truncated,
		&env.Corr.TraceID,
		&env.Corr.SpanID,
		&env.Corr.CausationID,
		&env.Corr.CorrelationID,
		&env.Corr.ThreadID,
		&env.Corr.IdempotencyKey,
		&env.Corr.Depth,
		&env.Actor.Kind,
		&env.Actor.URN,
		&env.Actor.Verified,
		&env.Actor.OnBehalfOf,
		&recorded,
		&env.Epoch,
		&env.RetentionClass,
		&env.RetentionPolicyID,
		&env.SchemaVersion,
		&env.App,
		&env.Subject,
		&env.TurnID,
		&env.ParentID,
		&rawOffset,
		&env.Process.PID,
		&env.Process.Provider,
		&env.Process.Runtime,
		&env.Process.ProviderSessionID,
		&env.Source.Channel,
		&env.Source.Confidence,
		&env.Sequence,
		&env.Generation,
		&env.Corr.Traceparent,
		&env.Corr.InReplyTo,
		&env.Actor.Role,
		&delegation,
		&origin,
		&env.PayloadDigest,
	); err != nil {
		return events.Event{}, err
	}
	var err error
	if e.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
		return events.Event{}, fmt.Errorf("event %d origin time: %w", e.Seq, err)
	}
	if origin != "" {
		if env.OriginAt, err = time.Parse(time.RFC3339Nano, origin); err != nil {
			return events.Event{}, fmt.Errorf("event %d origin metadata: %w", e.Seq, err)
		}
		e.At = env.OriginAt
	}
	if recorded != "" {
		if env.RecordedAt, err = time.Parse(time.RFC3339Nano, recorded); err != nil {
			return events.Event{}, fmt.Errorf("event %d recorded time: %w", e.Seq, err)
		}
	}
	if rawOffset.Valid {
		env.RawOffset = &rawOffset.Int64
	}
	if labels != "" {
		if err := json.Unmarshal([]byte(labels), &env.Class.Labels); err != nil {
			return events.Event{}, fmt.Errorf("event %d labels: %w", e.Seq, err)
		}
	}
	if delegation != "" {
		if err := json.Unmarshal([]byte(delegation), &env.Actor.OnBehalfOfChain); err != nil {
			return events.Event{}, fmt.Errorf("event %d delegation: %w", e.Seq, err)
		}
	}
	e.LogicalAgentID = env.Dims.LogicalAgentID
	return e, nil
}
