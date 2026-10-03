package fabricstore

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hollis-labs/substrate/mesh"
)

// EnrollmentRecord is a consistent persisted view, with no publication claim.
type EnrollmentRecord struct {
	Actor Record[Actor]
	Agent *Record[mesh.Agent]
}

func (tx *Tx) Actor(urn mesh.URN) (Record[Actor], error) {
	return read[Actor](tx.ctx, tx.conn, actors, string(urn))
}
func (tx *Tx) Agent(urn mesh.URN) (Record[mesh.Agent], error) {
	return read[mesh.Agent](tx.ctx, tx.conn, agents, string(urn))
}
func (tx *Tx) BindingHead(urn mesh.URN) (Record[BindingHead], error) {
	return read[BindingHead](tx.ctx, tx.conn, heads, string(urn))
}
func (tx *Tx) Receipt(id string) (Record[ImportReceipt], error) {
	return read[ImportReceipt](tx.ctx, tx.conn, receipts, id)
}

// Receipts reads a bounded import batch in one snapshot. An atomic concurrent
// apply therefore cannot appear as a partially committed batch to a retry.
func (r *Repository) Receipts(ctx context.Context, ids []string) ([]Record[ImportReceipt], error) {
	if len(ids) < 1 || len(ids) > 100 {
		return nil, ErrInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sqlReadOnly)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	result := []Record[ImportReceipt]{}
	for _, id := range ids {
		receipt, err := read[ImportReceipt](ctx, tx, receipts, id)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, receipt)
	}
	return result, tx.Commit()
}

// Enrollments pages by the original, byte-preserved URN in one read snapshot.
// No legacy registration is consulted. The host controls visibility separately.
func (r *Repository) Enrollments(ctx context.Context, owner, after mesh.URN, limit int) ([]EnrollmentRecord, error) {
	if limit < 1 || limit > 100 {
		return nil, ErrInvalid
	}
	tx, err := r.db.BeginTx(ctx, &sqlReadOnly)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT p.record_json,p.record_version,a.record_json,a.record_version
 FROM fabric_actors p LEFT JOIN fabric_agents a ON a.record_key=p.record_key
 WHERE json_extract(p.record_json,'$.owner')=? AND p.record_key>? ORDER BY p.record_key COLLATE BINARY LIMIT ?`, string(owner), string(after), limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	records := []EnrollmentRecord{}
	for rows.Next() {
		var record EnrollmentRecord
		var actor string
		var agent *string
		var version *int64
		if err := rows.Scan(&actor, &record.Actor.Version, &agent, &version); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(actor), &record.Actor.Value); err != nil {
			return nil, err
		}
		if agent != nil {
			record.Agent = &Record[mesh.Agent]{Version: *version}
			if err := json.Unmarshal([]byte(*agent), &record.Agent.Value); err != nil {
				return nil, err
			}
		}
		if (record.Actor.Value.Kind == mesh.ActorAgent) != (record.Agent != nil) {
			return nil, invalid("actor enrollment is incomplete")
		}
		if record.Agent != nil && (record.Agent.Value.URN != record.Actor.Value.URN || record.Agent.Value.Owner != record.Actor.Value.Owner || record.Agent.Value.Lifecycle != record.Actor.Value.Lifecycle) {
			return nil, invalid("enrollment records disagree")
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return records, tx.Commit()
}
