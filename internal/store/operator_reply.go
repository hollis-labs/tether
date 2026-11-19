package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// Operator-attested replies (CW-20261114-0152): challenge, one-use proof and
// answer-reservation storage, and the approved trust-root digest.

var (
	ErrTooManyChallenges = errors.New("too_many_challenges")
	ErrChallengeUnknown  = errors.New("challenge_unknown")
	ErrReplayed          = errors.New("replayed")
	ErrAlreadyAnswered   = errors.New("already_answered")
)

// OperatorReplyChallenge is one stored challenge.
type OperatorReplyChallenge struct {
	ID          string
	NonceSHA256 string
	MessageID   string
	Option      string
	Binding     string
	BodySHA256  string
	DeviceID    string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	ConsumedAt  *time.Time
}

func fmtTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// CreateOperatorReplyChallenge stores c unless its device already has limit
// open (unconsumed, unexpired) challenges.
func (s *Store) CreateOperatorReplyChallenge(ctx context.Context, c OperatorReplyChallenge, limit int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var open int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM operator_reply_challenges WHERE device_id=? AND consumed_at IS NULL AND expires_at > ?`, c.DeviceID, fmtTime(c.CreatedAt)).Scan(&open); err != nil {
		return err
	}
	if open >= limit {
		return ErrTooManyChallenges
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operator_reply_challenges(challenge_id,nonce_sha256,message_id,option,binding,body_sha256,device_id,created_at,expires_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		c.ID, c.NonceSHA256, c.MessageID, c.Option, c.Binding, c.BodySHA256, c.DeviceID, fmtTime(c.CreatedAt), fmtTime(c.ExpiresAt)); err != nil {
		return err
	}
	return tx.Commit()
}

// OperatorReplyChallenge reads a challenge; ErrChallengeUnknown when absent.
func (s *Store) OperatorReplyChallenge(ctx context.Context, id string) (OperatorReplyChallenge, error) {
	var c OperatorReplyChallenge
	var created, expires string
	var consumed sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT challenge_id,nonce_sha256,message_id,option,binding,body_sha256,device_id,created_at,expires_at,consumed_at FROM operator_reply_challenges WHERE challenge_id=?`, id).
		Scan(&c.ID, &c.NonceSHA256, &c.MessageID, &c.Option, &c.Binding, &c.BodySHA256, &c.DeviceID, &created, &expires, &consumed)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrChallengeUnknown
	}
	if err != nil {
		return c, err
	}
	c.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	c.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires)
	if consumed.Valid {
		t, _ := time.Parse(time.RFC3339Nano, consumed.String)
		c.ConsumedAt = &t
	}
	return c, nil
}

// ConsumeOperatorReplyChallenge marks a challenge consumed; it reports false
// when it already was. Any reply attempt naming the challenge consumes it,
// whether the attempt then passes or fails.
func (s *Store) ConsumeOperatorReplyChallenge(ctx context.Context, id string, now time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE operator_reply_challenges SET consumed_at=? WHERE challenge_id=? AND consumed_at IS NULL`, fmtTime(now), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OperatorReplyProofUsed reports whether a jti or elevation_id was spent.
func (s *Store) OperatorReplyProofUsed(ctx context.Context, jti, elevationID string) (bool, error) {
	var used bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operator_reply_proofs WHERE jti=? OR elevation_id=?)`, jti, elevationID).Scan(&used)
	return used, err
}

// operatorAnswered is the already-answered rule: a stored reply to messageID
// from one of the operator's address forms (the pending-replies settle rule),
// or a reserved attested answer.
func operatorAnswered(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, messageID string, operatorForms []string) (bool, error) {
	args := []any{messageID, messageID}
	marks := make([]string, 0, len(operatorForms))
	for _, f := range operatorForms {
		marks = append(marks, "?")
		args = append(args, f)
	}
	if len(marks) == 0 {
		marks = append(marks, "NULL")
	}
	var answered bool
	//nolint:gosec // G202: marks are ? placeholders; values are bound via args
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM operator_reply_answers WHERE message_id=?) OR EXISTS(SELECT 1 FROM messages WHERE in_reply_to=? AND from_urn IN (`+strings.Join(marks, ",")+`))`, args...).Scan(&answered)
	return answered, err
}

// OperatorAnswered reports whether the operator already answered messageID.
func (s *Store) OperatorAnswered(ctx context.Context, messageID string, operatorForms []string) (bool, error) {
	return operatorAnswered(ctx, s.db, messageID, operatorForms)
}

// OperatorReplyReservation is what an accepted attested reply spends.
type OperatorReplyReservation struct {
	MessageID   string
	ChallengeID string
	JTI         string
	ElevationID string
	At          time.Time
}

// ReserveOperatorReply is the reply-time transaction: it re-checks that the
// operator has not answered the parent (ErrAlreadyAnswered), records the jti
// and elevation_id (ErrReplayed when either was spent: UNIQUE makes this the
// guard against two concurrent replies), and reserves the parent's one
// answer. The reply itself is stored after commit (the message store cannot
// join this transaction); ReleaseOperatorReply undoes only the reservation if
// that fails, so the proofs stay spent.
func (s *Store) ReserveOperatorReply(ctx context.Context, r OperatorReplyReservation, operatorForms []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	answered, err := operatorAnswered(ctx, tx, r.MessageID, operatorForms)
	if err != nil {
		return err
	}
	if answered {
		return ErrAlreadyAnswered
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operator_reply_proofs(jti,elevation_id,challenge_id,message_id,created_at) VALUES(?,?,?,?,?)`, r.JTI, r.ElevationID, r.ChallengeID, r.MessageID, fmtTime(r.At)); err != nil {
		if isUniqueViolation(err) {
			return ErrReplayed
		}
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO operator_reply_answers(message_id,challenge_id,created_at) VALUES(?,?,?)`, r.MessageID, r.ChallengeID, fmtTime(r.At)); err != nil {
		if isUniqueViolation(err) {
			return ErrAlreadyAnswered
		}
		return err
	}
	return tx.Commit()
}

// CompleteOperatorReply records the stored reply's id on its reservation.
func (s *Store) CompleteOperatorReply(ctx context.Context, messageID, replyID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE operator_reply_answers SET reply_id=? WHERE message_id=?`, replyID, messageID)
	return err
}

// ReleaseOperatorReply drops a reservation whose reply could not be stored.
func (s *Store) ReleaseOperatorReply(ctx context.Context, messageID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM operator_reply_answers WHERE message_id=? AND reply_id IS NULL`, messageID)
	return err
}

func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// OperatorAttestationTrust is the approved trust root's record.
type OperatorAttestationTrust struct {
	Digest     string
	KeyIDs     []string
	PlanID     string
	ApprovedAt time.Time
}

// ApprovedOperatorAttestation returns the approved trust root; ok is false
// when none was ever approved (the feature is off; only an absent block is
// acceptable).
func (s *Store) ApprovedOperatorAttestation(ctx context.Context) (OperatorAttestationTrust, bool, error) {
	var t OperatorAttestationTrust
	var ids, at string
	err := s.db.QueryRowContext(ctx, `SELECT digest,key_ids_json,plan_id,approved_at FROM operator_attestation_trust WHERE id=1`).Scan(&t.Digest, &ids, &t.PlanID, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return t, false, nil
	}
	if err != nil {
		return t, false, err
	}
	_ = json.Unmarshal([]byte(ids), &t.KeyIDs)
	t.ApprovedAt, _ = time.Parse(time.RFC3339Nano, at)
	return t, true, nil
}

// SetApprovedOperatorAttestation records an approved trust root (an
// operator-approved plan is applying it) and returns the previous record.
func (s *Store) SetApprovedOperatorAttestation(ctx context.Context, t OperatorAttestationTrust) (OperatorAttestationTrust, bool, error) {
	prev, had, err := s.ApprovedOperatorAttestation(ctx)
	if err != nil {
		return prev, had, err
	}
	ids, _ := json.Marshal(t.KeyIDs)
	_, err = s.db.ExecContext(ctx, `INSERT INTO operator_attestation_trust(id,digest,key_ids_json,plan_id,approved_at) VALUES(1,?,?,?,?)
        ON CONFLICT(id) DO UPDATE SET digest=excluded.digest,key_ids_json=excluded.key_ids_json,plan_id=excluded.plan_id,approved_at=excluded.approved_at`,
		t.Digest, string(ids), t.PlanID, fmtTime(t.ApprovedAt))
	return prev, had, err
}

// RestoreOperatorAttestation puts back a previous record (or none) after an
// apply that failed and restored the catalog files.
func (s *Store) RestoreOperatorAttestation(ctx context.Context, prev OperatorAttestationTrust, had bool) error {
	if !had {
		_, err := s.db.ExecContext(ctx, `DELETE FROM operator_attestation_trust WHERE id=1`)
		return err
	}
	_, _, err := s.SetApprovedOperatorAttestation(ctx, prev)
	return err
}
