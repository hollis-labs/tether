package observability

import "time"

// AuditEvent is one durable, sanitized AI gateway audit record.
type AuditEvent struct {
	ID               int64
	EventType        string
	RequestID        string
	SessionID        string
	CallerID         string
	Operation        string
	Provider         string
	Model            string
	PolicyVersion    string
	LatencyMs        int64
	Success          bool
	Refusal          string
	Error            string
	InputTokens      int
	OutputTokens     int
	CacheReadTokens  int
	CacheWriteTokens int
	ReasoningTokens  int
	CostKind         string
	UpstreamProvider string
	BilledCostUSD    *float64
	GenerationID     string

	EstimatedCostUSD float64
	RequestSummary   string
	ResponseSummary  string
	Timestamp        time.Time
}

// Recorder persists audit events.
type Recorder interface {
	RecordAIAuditEvent(ev AuditEvent) error
}
