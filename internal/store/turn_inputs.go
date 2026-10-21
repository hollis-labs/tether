package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Turn input vocabulary (migration 0097).
const (
	InputChatSend     = "chat_send"
	InputSessionTurn  = "session_turn"
	InputMCPSendTurn  = "mcp_send_turn"
	InputMailWake     = "mail_wake"
	InputRoutingReply = "routing_reply"
	InputSystem       = "system"
	ClassOperator     = "operator"
	ClassAgent        = "agent"
	ClassSystem       = "system"
	ClassUnknown      = "unknown"
	BasisPrincipal    = "authenticated_principal"
	BasisSystem       = "system_injection"
	BasisNone         = "none"
	InputSubmitting   = "submitting"
	InputAccepted     = "accepted"
	InputFailed       = "failed"
)

// TurnInput is one input handed to a session.
type TurnInput struct {
	ID          string
	SessionID   string
	Seq         int64
	Source      string
	SenderURN   string
	SenderClass string
	Basis       string
	RefID       string
	State       string
	// Queued is true when another input of the session was still unbound when
	// this one was recorded: which turn it joins cannot be proven by order.
	Queued      bool
	SubmittedAt time.Time
}

// TurnAudience is what a routed output records about the input that triggered
// its turn. Classes is filled only when the binding is proven; in every doubtful
// case it is empty and Audience is "unknown", so a consumer that filters on
// Classes never shows a guess.
type TurnAudience struct {
	Audience string   `json:"audience"`
	Basis    string   `json:"audience_basis"`
	InputIDs []string `json:"input_ids,omitempty"`
	Classes  []string `json:"input_classes,omitempty"`
}

// UnknownAudience is the audience of an output with no recorded input.
func UnknownAudience(basis string) TurnAudience {
	return TurnAudience{Audience: ClassUnknown, Basis: basis}
}

// RecordTurnInput stores a new input in state "submitting" and returns it with
// its sequence number and queued flag. The sequence and the queued test are read
// and written in one transaction.
func (s *Store) RecordTurnInput(ctx context.Context, in TurnInput) (TurnInput, error) {
	if in.SessionID == "" || in.Source == "" {
		return TurnInput{}, fmt.Errorf("turn input needs a session and a source")
	}
	if in.SenderClass == "" {
		in.SenderClass, in.Basis = ClassUnknown, BasisNone
	}
	if in.Basis == "" {
		in.Basis = BasisNone
	}
	in.ID = "ti_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	in.State = InputSubmitting
	in.SubmittedAt = time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TurnInput{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var unbound int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id=? AND turn_id IS NULL AND state IN ('submitting','accepted')`, in.SessionID).Scan(&unbound); err != nil {
		return TurnInput{}, err
	}
	in.Queued = unbound > 0
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0)+1 FROM turn_inputs WHERE session_id=?`, in.SessionID).Scan(&in.Seq); err != nil {
		return TurnInput{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO turn_inputs(input_id,session_id,seq,source,sender_urn,sender_class,basis,ref_id,state,queued,submitted_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		in.ID, in.SessionID, in.Seq, in.Source, nullString(in.SenderURN), in.SenderClass, in.Basis, nullString(in.RefID), in.State, boolInt(in.Queued), in.SubmittedAt.Format(time.RFC3339Nano))
	if err != nil {
		return TurnInput{}, err
	}
	return in, tx.Commit()
}

// SettleTurnInput moves an input out of "submitting": to "accepted" once the
// runtime took the text, or to "failed". It never changes a settled input.
func (s *Store) SettleTurnInput(ctx context.Context, id string, accepted bool) error {
	state, at := InputFailed, sql.NullString{}
	if accepted {
		state, at = InputAccepted, sql.NullString{String: time.Now().UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE turn_inputs SET state=?, accepted_at=? WHERE input_id=? AND state='submitting'`, state, at, id)
	return err
}

// BindTurnInput is called when a turn of the session begins to produce output
// (or a result arrives). It binds the oldest unbound, not-failed input to turnID,
// by Tether-observed order only, and returns what the output may say about it.
//
// The audience is the input's class only when the binding is provable: the input
// is accepted, was not queued behind another, and no other input is waiting.
// Otherwise the audience is "unknown" with the reason as the basis, and no class
// is exposed. Calling it again for the same turn returns the first answer.
func (s *Store) BindTurnInput(ctx context.Context, sessionID, turnID string) (TurnAudience, error) {
	if sessionID == "" || turnID == "" {
		return UnknownAudience("no_input"), nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TurnAudience{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var id, audience, basis string
	var classes sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT input_id, COALESCE(audience,''), COALESCE(audience_basis,''), classes FROM turn_inputs WHERE session_id=? AND turn_id=? ORDER BY seq LIMIT 1`, sessionID, turnID).Scan(&id, &audience, &basis, &classes)
	switch {
	case err == nil:
		out := TurnAudience{Audience: audience, Basis: basis, InputIDs: []string{id}}
		if classes.Valid && classes.String != "" {
			out.Classes = strings.Split(classes.String, ",")
		}
		return out, nil
	case !errors.Is(err, sql.ErrNoRows):
		return TurnAudience{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT input_id, state, queued, sender_class, basis FROM turn_inputs WHERE session_id=? AND turn_id IS NULL AND state IN ('submitting','accepted') ORDER BY seq`, sessionID)
	if err != nil {
		return TurnAudience{}, err
	}
	type candidate struct {
		id, state, class, basis string
		queued                  bool
	}
	var waiting []candidate
	for rows.Next() {
		var c candidate
		var q int
		if err := rows.Scan(&c.id, &c.state, &q, &c.class, &c.basis); err != nil {
			_ = rows.Close()
			return TurnAudience{}, err
		}
		c.queued = q != 0
		waiting = append(waiting, c)
	}
	if err := rows.Close(); err != nil {
		return TurnAudience{}, err
	}
	if len(waiting) == 0 {
		return UnknownAudience("no_input"), nil
	}
	first := waiting[0]
	out := TurnAudience{Audience: ClassUnknown, InputIDs: []string{first.id}}
	switch {
	case first.state == InputSubmitting:
		out.Basis = "submission_unsettled"
	case first.queued:
		out.Basis = "queued_input"
	case len(waiting) > 1:
		out.Basis = "queued_inputs_present"
	case first.class == ClassUnknown:
		out.Basis = first.basis
	default:
		out.Audience, out.Basis, out.Classes = first.class, first.basis, []string{first.class}
	}
	_, err = tx.ExecContext(ctx, `UPDATE turn_inputs SET turn_id=?, audience=?, audience_basis=?, classes=? WHERE input_id=? AND turn_id IS NULL`, turnID, out.Audience, out.Basis, nullString(strings.Join(out.Classes, ",")), first.id)
	if err != nil {
		return TurnAudience{}, err
	}
	return out, tx.Commit()
}

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SettleLaunchPromptInput settles the creation prompt of a session once its launch
// has finished: accepted if it launched, failed if not.
func (s *Store) SettleLaunchPromptInput(ctx context.Context, sessionID string, launched bool) error {
	state, at := InputFailed, sql.NullString{}
	if launched {
		state, at = InputAccepted, sql.NullString{String: time.Now().UTC().Format(time.RFC3339Nano), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE turn_inputs SET state=?, accepted_at=? WHERE session_id=? AND ref_id='launch_prompt' AND state='submitting'`, state, at, sessionID)
	return err
}

// BasisMailPrefix starts the basis of an input classified from a stored message: it
// records both the From address kind and the class of the verified principal.
const BasisMailPrefix = "mail_from="

// MailBasis is "mail_from=<from kind>;principal=<principal class>".
func MailBasis(fromKind, principalClass string) string {
	return BasisMailPrefix + fromKind + ";principal=" + principalClass
}
