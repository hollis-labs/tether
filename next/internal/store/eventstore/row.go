// Package eventstore is the class B store for the event log (port slice S2b-1, plan 13.1): the
// events table with its outbox, ingest digests, retention sweeps and tombstones, MCP proxy events,
// tool-call metrics and turn inputs. It reaches its tables only through the class B handle (or a
// named joint transaction from the S2a allowlist) and writes SQL that runs on SQLite and Postgres.
//
// The store speaks rows, not the domain event: Row has one field per column, and the event slice
// (S8) maps its envelope onto it. The NULL rules are the old store's (internal/store/events_envelope.go
// at main f6c37bfa): an empty string, a zero number and a false flag are stored as NULL, and read back
// as their zero value.
package eventstore

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Row is one events row. Times are UTC; on disk they are RFC 3339 text with nanoseconds, as the old
// store wrote them (the migrated rows keep that format, and the text compares in time order only
// within one format, so every writer must use it).
type Row struct {
	Seq         int64 // id, assigned by the database
	EventID     string
	Scope       string
	SessionID   string
	At          time.Time
	Kind        string
	PayloadJSON string
	Pending     bool // an outbox row not yet published

	// Dimensions (scope_*).
	EnvironmentID, Authority, WorkerID, Host, ProjectID, WorkstreamID string
	EpicID, SprintID, PlanID, PhaseID, TaskID, RunID                  string
	Attempt                                                           int
	LaunchID, AgentURN, LogicalAgentID, TeamRunID, Cell               string

	// Classification.
	Category, Severity, Sensitivity string
	PII                             bool
	Visibility, PayloadSchema       string
	Sanitized                       bool
	Redactions                      int
	LabelsJSON                      string // labels_json, the domain's encoding, kept verbatim
	ContentType                     string
	Truncated                       bool

	// Correlation.
	TraceID, SpanID, CausationID, CorrelationID, ThreadID, IdempotencyKey string
	Depth                                                                 int
	Traceparent, InReplyTo                                                string

	// Actor.
	ActorKind, ActorURN, ActorVerified, OnBehalfOf, ActorRole string
	OnBehalfOfJSON                                            string // on_behalf_of_json, verbatim

	RecordedAt                           time.Time // the log's own stamp, set by Insert
	Epoch                                int64     // written NULL by Insert, as the old store did
	RetentionClass, RetentionPolicyID    string    // set by Insert from the admission table
	SchemaVersion, ProducerApp, Subject  string
	TurnID, ParentID                     string
	RawOffset                            *int64 // nil = NULL (0 is a real offset)
	ProcessPID                           int
	ProcessProvider, ProcessRuntime      string
	ProviderSessionID                    string
	SourceChannel, SourceConfidence      string
	ProducerSequence, ProducerGeneration uint64 // stored as decimal text, NULL when 0
	OriginAt                             time.Time
	PayloadDigest                        string // set only by a tombstone; never written by Insert
	Summary                              string
}

// insertColumns are written by Insert, in the order Row.insertValues returns their values.
var insertColumns = []string{
	"scope", "session_id", "at", "kind", "payload_json", "event_id", "pending",
	"scope_environment_id", "scope_authority", "scope_worker_id", "scope_host", "scope_project_id",
	"scope_workstream_id", "scope_epic_id", "scope_sprint_id", "scope_plan_id", "scope_phase_id",
	"scope_task_id", "scope_run_id", "scope_attempt", "scope_launch_id", "scope_agent_urn",
	"scope_logical_agent_id", "scope_team_run_id", "scope_cell",
	"category", "severity", "sensitivity", "pii", "visibility", "payload_schema", "sanitized", "redactions", "labels_json",
	"content_type", "truncated",
	"trace_id", "span_id", "causation_id", "correlation_id", "thread_id", "idempotency_key", "depth",
	"actor_kind", "actor_urn", "actor_verified", "on_behalf_of",
	"recorded_at", "epoch",
	"retention_class", "retention_policy_id",
	"schema_version", "producer_app", "subject", "turn_id", "parent_id", "raw_offset",
	"process_pid", "process_provider", "process_runtime", "provider_session_id",
	"source_channel", "source_confidence", "producer_sequence", "producer_generation",
	"traceparent", "in_reply_to", "actor_role", "on_behalf_of_json", "origin_at", "summary",
}

func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullFlag(b bool) any {
	if b {
		return int64(1)
	}
	return nil
}

func nullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

func nullUint(n uint64) any {
	if n == 0 {
		return nil
	}
	return strconv.FormatUint(n, 10)
}

func formatTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (r Row) insertValues() []any {
	pending := int64(0)
	if r.Pending {
		pending = 1
	}
	var raw any
	if r.RawOffset != nil {
		raw = *r.RawOffset
	}
	return []any{
		r.Scope, nullText(r.SessionID), formatTime(r.At), r.Kind, nullText(r.PayloadJSON), r.EventID, pending,
		nullText(r.EnvironmentID), nullText(r.Authority), nullText(r.WorkerID), nullText(r.Host), nullText(r.ProjectID),
		nullText(r.WorkstreamID), nullText(r.EpicID), nullText(r.SprintID), nullText(r.PlanID), nullText(r.PhaseID),
		nullText(r.TaskID), nullText(r.RunID), nullInt(int64(r.Attempt)), nullText(r.LaunchID), nullText(r.AgentURN),
		nullText(r.LogicalAgentID), nullText(r.TeamRunID), nullText(r.Cell),
		nullText(r.Category), nullText(r.Severity), nullText(r.Sensitivity), nullFlag(r.PII), nullText(r.Visibility),
		nullText(r.PayloadSchema), nullFlag(r.Sanitized), nullInt(int64(r.Redactions)), nullText(r.LabelsJSON),
		nullText(r.ContentType), nullFlag(r.Truncated),
		nullText(r.TraceID), nullText(r.SpanID), nullText(r.CausationID), nullText(r.CorrelationID), nullText(r.ThreadID),
		nullText(r.IdempotencyKey), nullInt(int64(r.Depth)),
		nullText(r.ActorKind), nullText(r.ActorURN), nullText(r.ActorVerified), nullText(r.OnBehalfOf),
		formatTime(r.RecordedAt), nil,
		nullText(r.RetentionClass), nullText(r.RetentionPolicyID),
		nullText(r.SchemaVersion), nullText(r.ProducerApp), nullText(r.Subject), nullText(r.TurnID), nullText(r.ParentID), raw,
		nullInt(int64(r.ProcessPID)), nullText(r.ProcessProvider), nullText(r.ProcessRuntime), nullText(r.ProviderSessionID),
		nullText(r.SourceChannel), nullText(r.SourceConfidence), nullUint(r.ProducerSequence), nullUint(r.ProducerGeneration),
		nullText(r.Traceparent), nullText(r.InReplyTo), nullText(r.ActorRole), nullText(r.OnBehalfOfJSON), nullTime(r.OriginAt),
		nullText(r.Summary),
	}
}

// readColumns is the one SELECT list for every events read. NULLs decode to the zero value; the
// numeric-as-text columns are read as text and parsed, so the list is valid on both dialects.
var readColumns = strings.Join([]string{
	"id", "COALESCE(scope,'')", "COALESCE(session_id,'')", "COALESCE(at,'')", "COALESCE(kind,'')",
	"COALESCE(payload_json,'')", "COALESCE(event_id,'')", "COALESCE(pending,0)",
	"COALESCE(scope_environment_id,'')", "COALESCE(scope_authority,'')", "COALESCE(scope_worker_id,'')",
	"COALESCE(scope_host,'')", "COALESCE(scope_project_id,'')", "COALESCE(scope_workstream_id,'')",
	"COALESCE(scope_epic_id,'')", "COALESCE(scope_sprint_id,'')", "COALESCE(scope_plan_id,'')",
	"COALESCE(scope_phase_id,'')", "COALESCE(scope_task_id,'')", "COALESCE(scope_run_id,'')",
	"COALESCE(scope_attempt,0)", "COALESCE(scope_launch_id,'')", "COALESCE(scope_agent_urn,'')",
	"COALESCE(scope_logical_agent_id,'')", "COALESCE(scope_team_run_id,'')", "COALESCE(scope_cell,'')",
	"COALESCE(category,'')", "COALESCE(severity,'')", "COALESCE(sensitivity,'')", "COALESCE(pii,0)",
	"COALESCE(visibility,'')", "COALESCE(payload_schema,'')", "COALESCE(sanitized,0)", "COALESCE(redactions,0)",
	"COALESCE(labels_json,'')", "COALESCE(content_type,'')", "COALESCE(truncated,0)",
	"COALESCE(trace_id,'')", "COALESCE(span_id,'')", "COALESCE(causation_id,'')", "COALESCE(correlation_id,'')",
	"COALESCE(thread_id,'')", "COALESCE(idempotency_key,'')", "COALESCE(depth,0)",
	"COALESCE(actor_kind,'')", "COALESCE(actor_urn,'')", "COALESCE(actor_verified,'')", "COALESCE(on_behalf_of,'')",
	"COALESCE(recorded_at,'')", "COALESCE(epoch,0)", "COALESCE(retention_class,'')", "COALESCE(retention_policy_id,'')",
	"COALESCE(schema_version,'')", "COALESCE(producer_app,'')", "COALESCE(subject,'')", "COALESCE(turn_id,'')",
	"COALESCE(parent_id,'')", "raw_offset", "COALESCE(process_pid,0)", "COALESCE(process_provider,'')",
	"COALESCE(process_runtime,'')", "COALESCE(provider_session_id,'')", "COALESCE(source_channel,'')",
	"COALESCE(source_confidence,'')", "COALESCE(producer_sequence,'')", "COALESCE(producer_generation,'')",
	"COALESCE(traceparent,'')", "COALESCE(in_reply_to,'')", "COALESCE(actor_role,'')", "COALESCE(on_behalf_of_json,'')",
	"COALESCE(origin_at,'')", "COALESCE(payload_digest,'')", "COALESCE(summary,'')",
}, ", ")

type scanner interface{ Scan(...any) error }

func scanRow(s scanner) (Row, error) {
	var r Row
	var at, recorded, origin, seq, gen string
	var pending, pii, sanitized, truncated, attempt, redactions, depth, pid int64
	var raw sql.NullInt64
	if err := s.Scan(
		&r.Seq, &r.Scope, &r.SessionID, &at, &r.Kind, &r.PayloadJSON, &r.EventID, &pending,
		&r.EnvironmentID, &r.Authority, &r.WorkerID, &r.Host, &r.ProjectID, &r.WorkstreamID,
		&r.EpicID, &r.SprintID, &r.PlanID, &r.PhaseID, &r.TaskID, &r.RunID,
		&attempt, &r.LaunchID, &r.AgentURN, &r.LogicalAgentID, &r.TeamRunID, &r.Cell,
		&r.Category, &r.Severity, &r.Sensitivity, &pii, &r.Visibility, &r.PayloadSchema, &sanitized, &redactions,
		&r.LabelsJSON, &r.ContentType, &truncated,
		&r.TraceID, &r.SpanID, &r.CausationID, &r.CorrelationID, &r.ThreadID, &r.IdempotencyKey, &depth,
		&r.ActorKind, &r.ActorURN, &r.ActorVerified, &r.OnBehalfOf,
		&recorded, &r.Epoch, &r.RetentionClass, &r.RetentionPolicyID,
		&r.SchemaVersion, &r.ProducerApp, &r.Subject, &r.TurnID, &r.ParentID, &raw,
		&pid, &r.ProcessProvider, &r.ProcessRuntime, &r.ProviderSessionID,
		&r.SourceChannel, &r.SourceConfidence, &seq, &gen,
		&r.Traceparent, &r.InReplyTo, &r.ActorRole, &r.OnBehalfOfJSON, &origin, &r.PayloadDigest, &r.Summary,
	); err != nil {
		return Row{}, err
	}
	r.Pending, r.PII, r.Sanitized, r.Truncated = pending != 0, pii != 0, sanitized != 0, truncated != 0
	r.Attempt, r.Redactions, r.Depth, r.ProcessPID = int(attempt), int(redactions), int(depth), int(pid)
	if raw.Valid {
		v := raw.Int64
		r.RawOffset = &v
	}
	var err error
	if r.At, err = parseTime(at); err != nil {
		return Row{}, fmt.Errorf("event %d at: %w", r.Seq, err)
	}
	if r.RecordedAt, err = parseTime(recorded); err != nil {
		return Row{}, fmt.Errorf("event %d recorded_at: %w", r.Seq, err)
	}
	if r.OriginAt, err = parseTime(origin); err != nil {
		return Row{}, fmt.Errorf("event %d origin_at: %w", r.Seq, err)
	}
	if r.ProducerSequence, err = parseUint(seq); err != nil {
		return Row{}, fmt.Errorf("event %d producer_sequence: %w", r.Seq, err)
	}
	if r.ProducerGeneration, err = parseUint(gen); err != nil {
		return Row{}, fmt.Errorf("event %d producer_generation: %w", r.Seq, err)
	}
	return r, nil
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

func parseUint(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	return strconv.ParseUint(s, 10, 64)
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer func() { _ = rows.Close() }()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
