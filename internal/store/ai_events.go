package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/hollis-labs/tether/internal/llm/observability"
)

const aiEventsMaxRows = 2000

type AIEvent = observability.AuditEvent

type AIEventFilter struct {
	EventType  string
	Provider   string
	Model      string
	SessionID  string
	CallerID   string
	Limit      int
	Since      time.Time
	ErrorsOnly bool
}

type AIUsageFilter struct {
	Provider  string
	Model     string
	SessionID string
	CallerID  string
	Operation string
	Since     time.Time
}

type AIUsageBreakdown struct {
	Key              string
	Requests         int
	Successes        int
	Errors           int
	LatencyMs        int64
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	EstimatedCostUSD float64
}

type AIUsageSummary struct {
	Requests         int
	Successes        int
	Errors           int
	LatencyMs        int64
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	EstimatedCostUSD float64
	ByProvider       []AIUsageBreakdown
	ByModel          []AIUsageBreakdown
	ByOperation      []AIUsageBreakdown
}

func aiEventWhereClause(f AIEventFilter) (string, []any) {
	q := ` WHERE 1=1`
	var args []any
	if f.ErrorsOnly {
		q += " AND success = 0"
	}
	if f.EventType != "" {
		q += " AND event_type = ?"
		args = append(args, f.EventType)
	}
	if f.Provider != "" {
		q += " AND provider = ?"
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		q += " AND model = ?"
		args = append(args, f.Model)
	}
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.CallerID != "" {
		q += " AND caller_id = ?"
		args = append(args, f.CallerID)
	}
	if !f.Since.IsZero() {
		q += " AND timestamp > ?"
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	return q, args
}

func aiUsageWhereClause(f AIUsageFilter) (string, []any) {
	q := ` WHERE event_type = ?`
	args := []any{"chat"}
	if f.Provider != "" {
		q += " AND provider = ?"
		args = append(args, f.Provider)
	}
	if f.Model != "" {
		q += " AND model = ?"
		args = append(args, f.Model)
	}
	if f.SessionID != "" {
		q += " AND session_id = ?"
		args = append(args, f.SessionID)
	}
	if f.CallerID != "" {
		q += " AND caller_id = ?"
		args = append(args, f.CallerID)
	}
	if f.Operation != "" {
		q += " AND operation = ?"
		args = append(args, f.Operation)
	}
	if !f.Since.IsZero() {
		q += " AND timestamp > ?"
		args = append(args, f.Since.UTC().Format(time.RFC3339Nano))
	}
	return q, args
}

func (s *Store) RecordAIAuditEvent(ev observability.AuditEvent) error {
	success := 0
	if ev.Success {
		success = 1
	}
	ts := ev.Timestamp.UTC().Format(time.RFC3339Nano)
	if ts == "" || ev.Timestamp.IsZero() {
		ts = time.Now().UTC().Format(time.RFC3339Nano)
	}
	_, err := s.db.Exec(
		`INSERT INTO ai_events
		    (event_type, request_id, session_id, caller_id, operation, provider, model,
		     policy_version, latency_ms, success, refusal, error, input_tokens, output_tokens,
		     cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd,
		     request_summary, response_summary, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ev.EventType,
		nullIfEmpty(ev.RequestID),
		nullIfEmpty(ev.SessionID),
		nullIfEmpty(ev.CallerID),
		ev.Operation,
		nullIfEmpty(ev.Provider),
		nullIfEmpty(ev.Model),
		nullIfEmpty(ev.PolicyVersion),
		ev.LatencyMs,
		success,
		nullIfEmpty(ev.Refusal),
		nullIfEmpty(ev.Error),
		ev.InputTokens,
		ev.OutputTokens,
		ev.CacheReadTokens,
		ev.CacheWriteTokens,
		ev.ReasoningTokens,
		ev.EstimatedCostUSD,
		nullIfEmpty(ev.RequestSummary),
		nullIfEmpty(ev.ResponseSummary),
		ts,
	)
	if err != nil {
		return fmt.Errorf("insert ai event: %w", err)
	}
	_, err = s.db.Exec(
		`DELETE FROM ai_events WHERE id IN (
		     SELECT id FROM ai_events ORDER BY id ASC
		     LIMIT MAX(0, (SELECT COUNT(*) FROM ai_events) - ?)
		 )`,
		aiEventsMaxRows,
	)
	if err != nil {
		return fmt.Errorf("trim ai events: %w", err)
	}
	return nil
}

func (s *Store) QueryAIEvents(f AIEventFilter) ([]AIEvent, error) {
	limit := f.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 0 || limit > aiEventsMaxRows {
		limit = aiEventsMaxRows
	}
	q := `SELECT id, event_type, request_id, session_id, caller_id, operation, provider, model,
	             policy_version, latency_ms, success, refusal, error, input_tokens, output_tokens,
	             cache_read_tokens, cache_write_tokens, reasoning_tokens, estimated_cost_usd,
	             request_summary, response_summary, timestamp
	      FROM ai_events`
	where, args := aiEventWhereClause(f)
	q += where
	q += " ORDER BY id ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query ai events: %w", err)
	}
	defer rows.Close()

	var out []AIEvent
	for rows.Next() {
		var (
			ev             AIEvent
			requestID      sql.NullString
			sessionID      sql.NullString
			callerID       sql.NullString
			provider       sql.NullString
			model          sql.NullString
			policyVersion  sql.NullString
			refusal        sql.NullString
			errStr         sql.NullString
			requestSummary sql.NullString
			respSummary    sql.NullString
			success        int
			tsStr          string
		)
		if err := rows.Scan(
			&ev.ID, &ev.EventType, &requestID, &sessionID, &callerID, &ev.Operation,
			&provider, &model, &policyVersion, &ev.LatencyMs, &success, &refusal, &errStr,
			&ev.InputTokens, &ev.OutputTokens, &ev.CacheReadTokens, &ev.CacheWriteTokens,
			&ev.ReasoningTokens, &ev.EstimatedCostUSD, &requestSummary, &respSummary, &tsStr,
		); err != nil {
			return nil, fmt.Errorf("scan ai event: %w", err)
		}
		ev.RequestID = requestID.String
		ev.SessionID = sessionID.String
		ev.CallerID = callerID.String
		ev.Provider = provider.String
		ev.Model = model.String
		ev.PolicyVersion = policyVersion.String
		ev.Refusal = refusal.String
		ev.Error = errStr.String
		ev.RequestSummary = requestSummary.String
		ev.ResponseSummary = respSummary.String
		ev.Success = success == 1
		ts, parseErr := time.Parse(time.RFC3339Nano, tsStr)
		if parseErr != nil {
			ts, _ = time.Parse(time.RFC3339, tsStr)
		}
		ev.Timestamp = ts
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ai events rows: %w", err)
	}
	return out, nil
}

func (s *Store) QueryAIUsageSummary(f AIUsageFilter) (AIUsageSummary, error) {
	var out AIUsageSummary
	where, args := aiUsageWhereClause(f)

	totalQ := `SELECT COUNT(*),
	                  SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END),
	                  SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
	                  COALESCE(SUM(latency_ms), 0),
	                  COALESCE(SUM(input_tokens), 0),
	                  COALESCE(SUM(output_tokens), 0),
	                  COALESCE(SUM(cache_read_tokens), 0),
	                  COALESCE(SUM(cache_write_tokens), 0),
	                  COALESCE(SUM(reasoning_tokens), 0),
	                  COALESCE(SUM(estimated_cost_usd), 0)
	           FROM ai_events` + where
	if err := s.db.QueryRow(totalQ, args...).Scan(
		&out.Requests,
		&out.Successes,
		&out.Errors,
		&out.LatencyMs,
		&out.InputTokens,
		&out.OutputTokens,
		&out.CacheReadTokens,
		&out.CacheWriteTokens,
		&out.ReasoningTokens,
		&out.EstimatedCostUSD,
	); err != nil {
		return AIUsageSummary{}, fmt.Errorf("query ai usage totals: %w", err)
	}

	var err error
	out.ByProvider, err = s.queryAIUsageBreakdown("provider", where, args)
	if err != nil {
		return AIUsageSummary{}, err
	}
	out.ByModel, err = s.queryAIUsageBreakdown("model", where, args)
	if err != nil {
		return AIUsageSummary{}, err
	}
	out.ByOperation, err = s.queryAIUsageBreakdown("operation", where, args)
	if err != nil {
		return AIUsageSummary{}, err
	}

	return out, nil
}

func (s *Store) queryAIUsageBreakdown(groupBy, where string, args []any) ([]AIUsageBreakdown, error) {
	q := fmt.Sprintf(`SELECT COALESCE(%s, ''),
	                         COUNT(*),
	                         SUM(CASE WHEN success = 1 THEN 1 ELSE 0 END),
	                         SUM(CASE WHEN success = 0 THEN 1 ELSE 0 END),
	                         COALESCE(SUM(latency_ms), 0),
	                         COALESCE(SUM(input_tokens), 0),
	                         COALESCE(SUM(output_tokens), 0),
	                         COALESCE(SUM(cache_read_tokens), 0),
	                         COALESCE(SUM(cache_write_tokens), 0),
	                         COALESCE(SUM(reasoning_tokens), 0),
	                         COALESCE(SUM(estimated_cost_usd), 0)
	                  FROM ai_events%s
	                  GROUP BY %s
	                  ORDER BY COUNT(*) DESC, %s ASC`, groupBy, where, groupBy, groupBy)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("query ai usage %s breakdown: %w", groupBy, err)
	}
	defer rows.Close()

	var out []AIUsageBreakdown
	for rows.Next() {
		var row AIUsageBreakdown
		if err := rows.Scan(
			&row.Key,
			&row.Requests,
			&row.Successes,
			&row.Errors,
			&row.LatencyMs,
			&row.InputTokens,
			&row.OutputTokens,
			&row.CacheReadTokens,
			&row.CacheWriteTokens,
			&row.ReasoningTokens,
			&row.EstimatedCostUSD,
		); err != nil {
			return nil, fmt.Errorf("scan ai usage %s breakdown: %w", groupBy, err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("ai usage %s breakdown rows: %w", groupBy, err)
	}
	return out, nil
}
