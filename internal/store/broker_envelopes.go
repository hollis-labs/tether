package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/chrispian/agent-mux/internal/broker"
)

// CreateEnvelope inserts a new broker_envelopes row. id + created_at
// are required; all other fields are optional. Priority defaults to 0.
func (s *Store) CreateEnvelope(e broker.Envelope) error {
	if e.ID == "" {
		return errors.New("envelope id required")
	}
	if e.CreatedAt == "" {
		return errors.New("envelope created_at required")
	}
	_, err := s.db.Exec(
		`INSERT INTO broker_envelopes (
            id, sender, recipient, workflow_id, correlation_id,
            message_type, priority, payload, created_at,
            delivered_at, consumed_at, audit_json
        ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID,
		nullIfEmpty(e.Sender), nullIfEmpty(e.Recipient),
		nullIfEmpty(e.WorkflowID), nullIfEmpty(e.CorrelationID),
		nullIfEmpty(e.MessageType), e.Priority,
		nullIfEmpty(e.Payload), e.CreatedAt,
		nullIfEmpty(e.DeliveredAt), nullIfEmpty(e.ConsumedAt),
		nullIfEmpty(e.AuditJSON),
	)
	if err != nil {
		return fmt.Errorf("insert envelope %q: %w", e.ID, err)
	}
	return nil
}

// GetEnvelope fetches a single envelope by id. Returns sql.ErrNoRows
// if no such row exists.
func (s *Store) GetEnvelope(id string) (*broker.Envelope, error) {
	var (
		e                                                                                    broker.Envelope
		sender, recipient, workflow, corr, msgType, payload, delivered, consumed, auditJSON sql.NullString
		priority                                                                              sql.NullInt64
	)
	err := s.db.QueryRow(
		`SELECT id, sender, recipient, workflow_id, correlation_id,
                message_type, priority, payload, created_at,
                delivered_at, consumed_at, audit_json
           FROM broker_envelopes WHERE id=?`,
		id,
	).Scan(&e.ID, &sender, &recipient, &workflow, &corr, &msgType,
		&priority, &payload, &e.CreatedAt, &delivered, &consumed, &auditJSON)
	if err != nil {
		return nil, err
	}
	e.Sender = sender.String
	e.Recipient = recipient.String
	e.WorkflowID = workflow.String
	e.CorrelationID = corr.String
	e.MessageType = msgType.String
	if priority.Valid {
		e.Priority = int(priority.Int64)
	}
	e.Payload = payload.String
	e.DeliveredAt = delivered.String
	e.ConsumedAt = consumed.String
	e.AuditJSON = auditJSON.String
	return &e, nil
}

// ListEnvelopesByRecipient returns undelivered envelopes for the given
// recipient in FIFO order (oldest created_at first). "Undelivered"
// means delivered_at IS NULL. Sprint v003-05 will generalise this into
// a fetch-and-mark-delivered primitive.
func (s *Store) ListEnvelopesByRecipient(recipient string) ([]broker.Envelope, error) {
	rows, err := s.db.Query(
		`SELECT id, sender, recipient, workflow_id, correlation_id,
                message_type, priority, payload, created_at,
                delivered_at, consumed_at, audit_json
           FROM broker_envelopes
          WHERE recipient=? AND delivered_at IS NULL
          ORDER BY created_at ASC`,
		recipient,
	)
	if err != nil {
		return nil, fmt.Errorf("list envelopes by recipient: %w", err)
	}
	defer rows.Close()
	return scanEnvelopes(rows)
}

// ListEnvelopesByWorkflow returns every envelope in the workflow
// ordered by created_at ascending — chronological conversation flow.
// A correlation_id filter is applied when non-empty.
func (s *Store) ListEnvelopesByWorkflow(workflowID, correlationID string) ([]broker.Envelope, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if correlationID == "" {
		rows, err = s.db.Query(
			`SELECT id, sender, recipient, workflow_id, correlation_id,
                    message_type, priority, payload, created_at,
                    delivered_at, consumed_at, audit_json
               FROM broker_envelopes
              WHERE workflow_id=? ORDER BY created_at ASC`,
			workflowID,
		)
	} else {
		rows, err = s.db.Query(
			`SELECT id, sender, recipient, workflow_id, correlation_id,
                    message_type, priority, payload, created_at,
                    delivered_at, consumed_at, audit_json
               FROM broker_envelopes
              WHERE workflow_id=? AND correlation_id=?
              ORDER BY created_at ASC`,
			workflowID, correlationID,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("list envelopes by workflow: %w", err)
	}
	defer rows.Close()
	return scanEnvelopes(rows)
}

func scanEnvelopes(rows *sql.Rows) ([]broker.Envelope, error) {
	var out []broker.Envelope
	for rows.Next() {
		var (
			e                                                                                      broker.Envelope
			sender, recipient, workflow, corr, msgType, payload, delivered, consumed, auditJSON sql.NullString
			priority                                                                                sql.NullInt64
		)
		if err := rows.Scan(&e.ID, &sender, &recipient, &workflow, &corr, &msgType,
			&priority, &payload, &e.CreatedAt, &delivered, &consumed, &auditJSON); err != nil {
			return nil, fmt.Errorf("scan envelope: %w", err)
		}
		e.Sender = sender.String
		e.Recipient = recipient.String
		e.WorkflowID = workflow.String
		e.CorrelationID = corr.String
		e.MessageType = msgType.String
		if priority.Valid {
			e.Priority = int(priority.Int64)
		}
		e.Payload = payload.String
		e.DeliveredAt = delivered.String
		e.ConsumedAt = consumed.String
		e.AuditJSON = auditJSON.String
		out = append(out, e)
	}
	return out, rows.Err()
}
