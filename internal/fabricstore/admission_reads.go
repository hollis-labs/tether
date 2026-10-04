package fabricstore

import (
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/hollis-labs/substrate/mesh"
	"time"
)

// Admission and execution reads use the same reserved writer as their CAS writes.
func (tx *Tx) Admission(caller mesh.URN, key string) (Record[Admission], error) {
	return read[Admission](tx.ctx, tx.conn, admissions, tuple(string(caller), key))
}
func (tx *Tx) Session(urn mesh.URN) (Record[mesh.Session], error) {
	return read[mesh.Session](tx.ctx, tx.conn, sessions, string(urn))
}
func (tx *Tx) Instance(id string) (Record[mesh.AgentInstance], error) {
	return read[mesh.AgentInstance](tx.ctx, tx.conn, instances, id)
}
func (tx *Tx) BindingEvent(id string) (Record[BindingEvent], error) {
	return read[BindingEvent](tx.ctx, tx.conn, history, id)
}

// Event reads one durable outbox entry within the same reserved writer.
func (tx *Tx) Event(id string) (Event, error) {
	var result Event
	var raw, created string
	err := tx.conn.QueryRowContext(tx.ctx, `SELECT cursor,event_id,aggregate_urn,event_type,payload_json,created_at FROM fabric_outbox WHERE event_id=?`, id).Scan(&result.Cursor, &result.ID, &result.AggregateURN, &result.Type, &raw, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return result, ErrNotFound
	}
	if err != nil {
		return result, err
	}
	result.Payload = json.RawMessage(raw)
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return result, err
}
