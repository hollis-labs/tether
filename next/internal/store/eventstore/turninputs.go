package eventstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Turn input vocabulary (turn_inputs.source, sender_class, basis and state), unchanged from the old
// store (internal/store/turn_inputs.go).
const (
	InputChatSend     = "chat_send"
	InputSessionTurn  = "session_turn"
	InputMCPSendTurn  = "mcp_send_turn"
	InputMailWake     = "mail_wake"
	InputRoutingReply = "routing_reply"
	InputSystem       = "system"

	ClassOperator = "operator"
	ClassAgent    = "agent"
	ClassSystem   = "system"
	ClassUnknown  = "unknown"

	BasisPrincipal      = "authenticated_principal"
	BasisStampedSender  = "stamped_message_sender"
	BasisSharedOperator = "shared_operator_credential"
	BasisSystem         = "system_injection"
	BasisNone           = "none"

	InputSubmitting = "submitting"
	InputAccepted   = "accepted"
	InputFailed     = "failed"
)

// TurnInput is one input submitted to a session's turn.
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
	Queued      bool
	SubmittedAt time.Time
}

// TurnAudience is who a turn's output answers, as far as the inputs prove it.
type TurnAudience struct {
	Audience string   `json:"audience"`
	Basis    string   `json:"audience_basis"`
	InputIDs []string `json:"input_ids,omitempty"`
	Classes  []string `json:"input_classes,omitempty"`
	Sources  []string `json:"input_sources,omitempty"`
}

// UnknownAudience is the audience when no input proves one.
func UnknownAudience(basis string) TurnAudience {
	return TurnAudience{Audience: ClassUnknown, Basis: basis}
}

func unprovenBinding(basis string) bool {
	switch basis {
	case "submission_unsettled", "queued_input", "queued_inputs_present", "binding_unavailable", "no_input":
		return true
	}
	return false
}

func newInputID() string { return "ti_" + strings.ReplaceAll(uuid.NewString(), "-", "") }

// lockSession serializes turn input allocation for one session inside the transaction. SQLite has
// one writer, so its write transaction already does; on Postgres two transactions could read the
// same MAX(seq) (dialect audit: MAX+1 allocation with no unique index), so a transaction-scoped
// advisory lock on the session is taken first.
func lockSession(ctx context.Context, tx store.Tx, sessionID string) error {
	if !isPostgres(tx) {
		return nil
	}
	_, err := tx.ExecContext(ctx, rebind(tx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`), "turn_inputs:"+sessionID)
	return err
}

// RecordTurnInput records a submitted input with the next per-session seq. It is queued when an
// earlier input of the session is still unbound and live.
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
	in.ID, in.State, in.SubmittedAt = s.NewInputID(), InputSubmitting, s.now()
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		if err := lockSession(ctx, tx, in.SessionID); err != nil {
			return err
		}
		var unbound int
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COUNT(*) FROM turn_inputs WHERE session_id = ? AND turn_id IS NULL AND state IN ('submitting','accepted')`),
			in.SessionID).Scan(&unbound); err != nil {
			return err
		}
		in.Queued = unbound > 0
		if err := tx.QueryRowContext(ctx, rebind(tx, `SELECT COALESCE(MAX(seq),0)+1 FROM turn_inputs WHERE session_id = ?`), in.SessionID).Scan(&in.Seq); err != nil {
			return err
		}
		queued := int64(0)
		if in.Queued {
			queued = 1
		}
		_, err := tx.ExecContext(ctx, rebind(tx, `INSERT INTO turn_inputs (input_id, session_id, seq, source, sender_urn, sender_class, basis, ref_id, state, queued, submitted_at)
 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`),
			in.ID, in.SessionID, in.Seq, in.Source, nullText(in.SenderURN), in.SenderClass, in.Basis, nullText(in.RefID), in.State, queued, formatTime(in.SubmittedAt))
		return err
	})
	if err != nil {
		return TurnInput{}, err
	}
	return in, nil
}

func (s *Store) settle(ctx context.Context, where string, arg any, accepted bool) error {
	state := InputFailed
	var at any
	if accepted {
		state, at = InputAccepted, formatTime(s.now())
	}
	return s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		_, err := tx.ExecContext(ctx, rebind(tx, `UPDATE turn_inputs SET state = ?, accepted_at = ? WHERE `+where+` AND state = 'submitting'`), state, at, arg)
		return err
	})
}

// SettleTurnInput marks a submitting input accepted (with accepted_at) or failed.
func (s *Store) SettleTurnInput(ctx context.Context, id string, accepted bool) error {
	return s.settle(ctx, "input_id = ?", id, accepted)
}

// SettleLaunchPromptInput settles a session's launch prompt input.
func (s *Store) SettleLaunchPromptInput(ctx context.Context, sessionID string, launched bool) error {
	return s.settle(ctx, "ref_id = 'launch_prompt' AND session_id = ?", sessionID, launched)
}

// BindTurnInput binds the turn to its proving input and returns the audience. A turn already
// bound returns its stored audience. Otherwise the session's oldest live unbound input is bound,
// and the audience is proven only when it is settled, not queued and alone; every other case binds
// with an unproven basis.
func (s *Store) BindTurnInput(ctx context.Context, sessionID, turnID string) (TurnAudience, error) {
	if sessionID == "" || turnID == "" {
		return UnknownAudience("no_input"), nil
	}
	var out TurnAudience
	err := s.b.WriteTx(ctx, func(ctx context.Context, tx store.Tx) error {
		var id, source, audience, basis string
		var classes sql.NullString
		err := tx.QueryRowContext(ctx, rebind(tx, `SELECT input_id, source, COALESCE(audience,''), COALESCE(audience_basis,''), classes
 FROM turn_inputs WHERE session_id = ? AND turn_id = ? ORDER BY seq LIMIT 1`), sessionID, turnID).Scan(&id, &source, &audience, &basis, &classes)
		switch {
		case err == nil:
			out = TurnAudience{Audience: audience, Basis: basis, InputIDs: []string{id}}
			if classes.Valid && classes.String != "" {
				out.Classes = strings.Split(classes.String, ",")
			}
			if !unprovenBinding(basis) && source != "" {
				out.Sources = []string{source}
			}
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		rows, err := tx.QueryContext(ctx, rebind(tx, `SELECT input_id, source, state, queued, sender_class, basis
 FROM turn_inputs WHERE session_id = ? AND turn_id IS NULL AND state IN ('submitting','accepted') ORDER BY seq`), sessionID)
		if err != nil {
			return err
		}
		type candidate struct {
			id, source, state, class, basis string
			queued                          bool
		}
		var waiting []candidate
		for rows.Next() {
			var c candidate
			var q int64
			if err := rows.Scan(&c.id, &c.source, &c.state, &q, &c.class, &c.basis); err != nil {
				_ = rows.Close()
				return err
			}
			c.queued = q != 0
			waiting = append(waiting, c)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(waiting) == 0 {
			out = UnknownAudience("no_input")
			return nil
		}
		first := waiting[0]
		out = TurnAudience{Audience: ClassUnknown, InputIDs: []string{first.id}}
		switch {
		case first.state == InputSubmitting:
			out.Basis = "submission_unsettled"
		case first.queued:
			out.Basis = "queued_input"
		case len(waiting) > 1:
			out.Basis = "queued_inputs_present"
		case first.class == ClassUnknown:
			out.Basis, out.Sources = first.basis, []string{first.source}
		default:
			out.Audience, out.Basis, out.Classes, out.Sources = first.class, first.basis, []string{first.class}, []string{first.source}
		}
		_, err = tx.ExecContext(ctx, rebind(tx, `UPDATE turn_inputs SET turn_id = ?, audience = ?, audience_basis = ?, classes = ? WHERE input_id = ? AND turn_id IS NULL`),
			turnID, out.Audience, out.Basis, nullText(strings.Join(out.Classes, ",")), first.id)
		return err
	})
	if err != nil {
		return TurnAudience{}, err
	}
	return out, nil
}
