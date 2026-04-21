// Package checkpoint holds the durable continuity-record model for
// logical agents. A Checkpoint captures "what did this agent do, where
// is it, what's next" at a moment in time so a future session can
// resume. v0.0.2 ships only the type + storage primitives (see
// internal/store/checkpoints.go); create-from-session and
// resume-to-session semantics arrive in Sprint v003-04.
//
// Payload fields are free-form strings — typically operator-authored
// prose or small JSON blobs. The schema deliberately doesn't pin their
// shape until v0.0.3 when the resume path clarifies requirements.
package checkpoint

// Checkpoint mirrors the checkpoints row. String fields map to nullable
// TEXT columns; empty string ↔ SQL NULL.
type Checkpoint struct {
	ID                  string
	LogicalAgentID      string
	TaskID              string
	WorkflowID          string
	Status              string
	CompletedWork       string
	PendingWork         string
	KeyDecisions        string
	ReferencedArtifacts string
	Summary             string
	NextRecommendation  string
	CreatedAt           string
	SourceSessionID     string
	// ProviderHintsJSON holds an opaque JSON blob from Session.CheckpointHints().
	// Empty string means no hints were provided. Round-tripped as-is; the daemon
	// does not interpret the content. See ADR 0015.
	ProviderHintsJSON string
}
