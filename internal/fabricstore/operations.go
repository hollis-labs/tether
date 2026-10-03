package fabricstore

import (
	"context"
	"encoding/json"
	"time"

	"github.com/hollis-labs/substrate/mesh"
)

// Admission stores the authenticated idempotency key and immutable request
// digest. Request/response content stays in its owning operation store.
type Admission struct {
	Caller        mesh.URN `json:"caller"`
	Key           string   `json:"key"`
	RequestDigest string   `json:"request_digest"`
	OperationRef  string   `json:"operation_ref"`
	InstanceID    string   `json:"instance_id,omitempty"`
	State         string   `json:"state"`
}

func (tx *Tx) PutAdmission(value Admission, expected int64) error {
	if value.Caller.Validate() != nil || value.Key == "" || value.RequestDigest == "" || value.OperationRef == "" || (value.State != "reserved" && value.State != "committed" && value.State != "rejected") {
		return invalid("admission fields are invalid")
	}
	if value.InstanceID != "" {
		if _, err := read[mesh.AgentInstance](tx.ctx, tx.conn, instances, value.InstanceID); err != nil {
			return err
		}
	}
	key := tuple(string(value.Caller), value.Key)
	if expected > 0 {
		old, err := read[Admission](tx.ctx, tx.conn, admissions, key)
		if err != nil {
			return err
		}
		if old.Value.RequestDigest != value.RequestDigest || old.Value.OperationRef != value.OperationRef || (old.Value.InstanceID != "" && old.Value.InstanceID != value.InstanceID) || (old.Value.State != "reserved" && old.Value.State != value.State) {
			return invalid("admission request/reservation is immutable")
		}
	}
	return put(tx, admissions, key, value, expected)
}
func (r *Repository) Admission(ctx context.Context, caller mesh.URN, key string) (Record[Admission], error) {
	return read[Admission](ctx, r.db, admissions, tuple(string(caller), key))
}

// MigrationCandidate records unresolved references, never raw catalog payload.
type MigrationCandidate struct {
	ID          string `json:"id"`
	LegacyRef   string `json:"legacy_ref"`
	EvidenceRef string `json:"evidence_ref"`
	Reason      string `json:"reason"`
	State       string `json:"state"`
}

func (tx *Tx) PutCandidate(value MigrationCandidate, expected int64) error {
	if value.ID == "" || value.LegacyRef == "" || value.EvidenceRef == "" || value.Reason == "" || (value.State != "pending" && value.State != "reviewed" && value.State != "excluded") {
		return invalid("candidate fields are invalid")
	}
	if expected > 0 {
		old, err := read[MigrationCandidate](tx.ctx, tx.conn, candidates, value.ID)
		if err != nil {
			return err
		}
		if old.Value.LegacyRef != value.LegacyRef || old.Value.EvidenceRef != value.EvidenceRef {
			return invalid("candidate provenance is immutable")
		}
	}
	return put(tx, candidates, value.ID, value, expected)
}
func (r *Repository) Candidate(ctx context.Context, id string) (Record[MigrationCandidate], error) {
	return read[MigrationCandidate](ctx, r.db, candidates, id)
}

type ImportReceipt struct {
	ID          string    `json:"id"`
	CandidateID string    `json:"candidate_id"`
	ActorURN    mesh.URN  `json:"actor_urn"`
	ApprovalRef string    `json:"approval_ref"`
	At          time.Time `json:"at"`
}

func (tx *Tx) AddReceipt(value ImportReceipt) error {
	if value.ID == "" || value.ApprovalRef == "" || value.At.IsZero() {
		return invalid("receipt fields are invalid")
	}
	candidate, err := read[MigrationCandidate](tx.ctx, tx.conn, candidates, value.CandidateID)
	if err != nil {
		return err
	}
	if candidate.Value.State != "reviewed" {
		return invalid("import candidate has not been reviewed")
	}
	if _, err := read[Actor](tx.ctx, tx.conn, actors, string(value.ActorURN)); err != nil {
		return err
	}
	return put(tx, receipts, value.ID, value, 0)
}
func (r *Repository) Receipt(ctx context.Context, id string) (Record[ImportReceipt], error) {
	return read[ImportReceipt](ctx, r.db, receipts, id)
}

type LegacyRef struct {
	Source    string   `json:"source"`
	Key       string   `json:"key"`
	ActorURN  mesh.URN `json:"actor_urn"`
	ReceiptID string   `json:"receipt_id"`
}

func (tx *Tx) AddLegacyRef(value LegacyRef) error {
	if value.Source == "" || value.Key == "" {
		return invalid("legacy reference fields are invalid")
	}
	receipt, err := read[ImportReceipt](tx.ctx, tx.conn, receipts, value.ReceiptID)
	if err != nil {
		return err
	}
	if receipt.Value.ActorURN != value.ActorURN {
		return invalid("legacy reference does not match receipt")
	}
	return put(tx, legacyRefs, tuple(value.Source, value.Key), value, 0)
}
func (r *Repository) LegacyRef(ctx context.Context, source, key string) (Record[LegacyRef], error) {
	return read[LegacyRef](ctx, r.db, legacyRefs, tuple(source, key))
}

type Event struct {
	Cursor       int64
	ID           string
	AggregateURN mesh.URN
	Type         string
	Payload      json.RawMessage
	CreatedAt    time.Time
	DeliveredAt  *time.Time
}

func (tx *Tx) AppendEvent(value Event) error {
	if value.Cursor != 0 || value.ID == "" || value.AggregateURN.Validate() != nil || value.Type == "" || !json.Valid(value.Payload) || value.CreatedAt.IsZero() || value.DeliveredAt != nil {
		return invalid("outbox event is invalid")
	}
	result, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO fabric_outbox(event_id,aggregate_urn,event_type,payload_json,created_at) VALUES (?,?,?,?,?) ON CONFLICT(event_id) DO NOTHING`, value.ID, string(value.AggregateURN), value.Type, string(value.Payload), value.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	return changed(result)
}

// Events uses the durable cursor for ordered replay, with bounded batches.
func (r *Repository) Events(ctx context.Context, after int64, limit int) ([]Event, error) {
	if after < 0 || limit < 1 || limit > 1000 {
		return nil, ErrInvalid
	}
	rows, err := r.db.QueryContext(ctx, `SELECT cursor,event_id,aggregate_urn,event_type,payload_json,created_at,delivered_at FROM fabric_outbox WHERE cursor>? ORDER BY cursor LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []Event{}
	for rows.Next() {
		var event Event
		var raw, created string
		var delivered *string
		if err := rows.Scan(&event.Cursor, &event.ID, &event.AggregateURN, &event.Type, &raw, &created, &delivered); err != nil {
			return nil, err
		}
		event.Payload = json.RawMessage(raw)
		event.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, err
		}
		if delivered != nil {
			timestamp, err := time.Parse(time.RFC3339Nano, *delivered)
			if err != nil {
				return nil, err
			}
			event.DeliveredAt = &timestamp
		}
		result = append(result, event)
	}
	return result, rows.Err()
}
func (tx *Tx) MarkDelivered(cursor int64, at time.Time) error {
	if cursor <= 0 || at.IsZero() {
		return ErrInvalid
	}
	result, err := tx.conn.ExecContext(tx.ctx, `UPDATE fabric_outbox SET delivered_at=? WHERE cursor=? AND delivered_at IS NULL`, at.UTC().Format(time.RFC3339Nano), cursor)
	if err != nil {
		return err
	}
	return changed(result)
}
