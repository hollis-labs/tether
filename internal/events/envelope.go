package events

import (
	"context"
	"fmt"
	"time"
)

// The persisted v1.1 envelope blocks (CW-20261016-0136, X1c). Field names follow
// the event stream contract (runtimeevents scope.md / blocks.go). Every field is
// optional: a producer fills what it can verify, and a value it cannot verify is
// left empty, never guessed. Nothing here is filled by the bus; the stamp stage
// (X4) and the producers do, and X1c only persists what an Event carries.

// ScopeDims is the closed set of scope dimensions (scope.md): low-cardinality
// identifiers only. It is not the legacy Event.Scope string.
type ScopeDims struct {
	EnvironmentID  string
	Authority      string
	WorkerID       string
	Host           string
	ProjectID      string
	WorkstreamID   string
	EpicID         string
	SprintID       string
	PlanID         string
	PhaseID        string
	TaskID         string
	RunID          string
	Attempt        int // 1-based; 0 = unknown
	LaunchID       string
	AgentURN       string
	LogicalAgentID string
	TeamRunID      string
}

// Class is the class block: what kind of event this is and how it may be handled.
type Class struct {
	Category      string
	Severity      string
	Sensitivity   string // public | private | secret
	PII           bool
	Visibility    string
	PayloadSchema string
	Sanitized     bool
	Redactions    int
	Labels        []string
}

// Corr is the correlation block.
type Corr struct {
	TraceID        string
	SpanID         string
	CausationID    string
	CorrelationID  string
	ThreadID       string
	IdempotencyKey string
	Depth          int
}

// Actor says who caused the event.
type Actor struct {
	Kind       string
	URN        string
	Verified   string // token | claimed | none
	OnBehalfOf string
}

// Envelope carries the v1.1 blocks of an Event.
type Envelope struct {
	Dims        ScopeDims
	Class       Class
	Corr        Corr
	Actor       Actor
	ContentType string
	Truncated   bool
	// RecordedAt and Epoch are assigned by the log when the row is written; a
	// producer-supplied value is ignored.
	RecordedAt time.Time
	Epoch      int64
	// RetentionClass and RetentionPolicyID are set at admission by the retention
	// lane. Empty means legacy / unclassified, which retention holds; it is never
	// derived at trim time.
	RetentionClass    string
	RetentionPolicyID string
}

// Retention classes (D7).
const (
	RetentionAudit    = "R-audit"
	RetentionState    = "R-state"
	RetentionActivity = "R-activity"
	RetentionDebug    = "R-debug"
)

// Validate checks the fields the schema constrains.
func (e Envelope) Validate() error {
	switch e.RetentionClass {
	case "", RetentionAudit, RetentionState, RetentionActivity, RetentionDebug:
	default:
		return fmt.Errorf("events: unknown retention class %q", e.RetentionClass)
	}
	if e.Dims.Attempt < 0 || e.Corr.Depth < 0 || e.Class.Redactions < 0 {
		return fmt.Errorf("events: negative attempt, depth or redactions")
	}
	return nil
}

// EnvelopePersister is a Persister that writes the whole envelope with the row
// and returns the stored event (Seq, At, EventID, Envelope.RecordedAt).
type EnvelopePersister interface {
	InsertEventEnvelope(ctx context.Context, e Event) (Event, error)
}
