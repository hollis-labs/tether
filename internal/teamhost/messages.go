package teamhost

import (
	"context"
	"database/sql"
	"errors"
	"reflect"

	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/substrate/mesh/teams"
)

func (h *Host) BindMessage(ctx context.Context, key, digest string) error {
	if key == "" || digest == "" {
		return errors.New("message: key and digest required")
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		var prior string
		err := conn.QueryRowContext(ctx, `SELECT digest FROM team_host_messages WHERE message_key=?`, key).Scan(&prior)
		if err == nil {
			if prior != digest {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_messages(message_key,digest) VALUES(?,?)`, key, digest)
		return err
	})
}
func loadDelivery(ctx context.Context, q rowReader, key string) (teams.Delivery, error) {
	var delivery teams.Delivery
	var payload []byte
	if err := q.QueryRowContext(ctx, `SELECT payload FROM team_host_deliveries WHERE delivery_key=?`, key).Scan(&payload); err != nil {
		return delivery, notFound(err)
	}
	return delivery, decode(payload, &delivery)
}
func (h *Host) GetDelivery(ctx context.Context, key string) (teams.Delivery, error) {
	return loadDelivery(ctx, h.db, key)
}
func (h *Host) DelegationState(ctx context.Context, key string) (mesh.TaskState, error) {
	var state mesh.TaskState
	err := h.db.QueryRowContext(ctx, `SELECT state FROM team_host_delegations WHERE delivery_key=?`, key).Scan(&state)
	return state, notFound(err)
}

// EndDelegation is a trusted host seam. A terminal state is first-wins, and
// cannot be reopened while a result races its acceptance transaction.
func (h *Host) EndDelegation(ctx context.Context, key string, state mesh.TaskState) error {
	if !state.Valid() || !state.Terminal() {
		return errors.New("delegation: terminal state required")
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		var old mesh.TaskState
		if err := conn.QueryRowContext(ctx, `SELECT state FROM team_host_delegations WHERE delivery_key=?`, key).Scan(&old); err != nil {
			return notFound(err)
		}
		if old.Terminal() {
			if old != state {
				return teams.ErrConflict
			}
			return nil
		}
		_, err := conn.ExecContext(ctx, `UPDATE team_host_delegations SET state=? WHERE delivery_key=?`, state, key)
		return err
	})
}
func activePairs(ctx context.Context, conn *sql.Conn, runID string, wanted ...teams.Member) error {
	var payload []byte
	if err := conn.QueryRowContext(ctx, `SELECT payload FROM team_rosters WHERE run_id=?`, runID).Scan(&payload); err != nil {
		return notFound(err)
	}
	var roster teams.Roster
	if err := decode(payload, &roster); err != nil {
		return err
	}
	for _, target := range wanted {
		found := false
		for _, m := range roster.Members {
			if m.ID == target.ID && m.Actor == target.Actor && m.SessionID == target.SessionID && m.Status == "active" {
				found = true
				break
			}
		}
		if !found {
			return teams.ErrUnavailable
		}
	}
	return nil
}

// SendMessage accepts a durable outbox entry. Acceptance, delegation state and
// reply member/session checks share one fenced transaction. Accepted replays
// precede liveness checks. FlushMessages is the explicit transport recovery seam;
// it never substitutes recipients or injects an at-idle reply immediately.
func (h *Host) SendMessage(ctx context.Context, d teams.Delivery) error {
	if d.IdempotencyKey == "" || d.Body == "" || d.From.Validate() != nil || d.Recipient.Actor.Validate() != nil || d.Recipient.ID == "" || d.Route.RunID == "" || d.Route.RosterVersion == 0 {
		return errors.New("delivery: retained plan required")
	}
	if d.Recipient.Kind == mesh.ActorAgent && d.Recipient.SessionID == "" {
		return teams.ErrUnavailable
	}
	if d.Delivery != "" && d.Delivery != mesh.DeliveryAtIdle {
		return teams.ErrUnsupported
	}
	payload, err := encode(d)
	if err != nil {
		return err
	}
	return h.tx(ctx, func(conn *sql.Conn) error {
		old, err := loadDelivery(ctx, conn, d.IdempotencyKey)
		if err == nil {
			prior, err := encode(old)
			if err != nil {
				return err
			}
			equal, err := same(prior, d)
			if err != nil {
				return err
			}
			if !equal {
				return teams.ErrConflict
			}
			return nil
		}
		if !errors.Is(err, teams.ErrNotFound) {
			return err
		}
		if d.Verb == mesh.Reply && d.Route.Verb == mesh.Delegate {
			original, err := loadDelivery(ctx, conn, d.InReplyTo)
			if err != nil {
				return err
			}
			if original.Verb != mesh.Delegate || d.From != original.Recipient.Actor || !reflect.DeepEqual(d.Recipient, original.Route.Sender) || !reflect.DeepEqual(d.Route, original.Route) || d.History != mesh.HistoryNone || d.Delivery != mesh.DeliveryAtIdle {
				return teams.ErrConflict
			}
			var state mesh.TaskState
			if err = conn.QueryRowContext(ctx, `SELECT state FROM team_host_delegations WHERE delivery_key=?`, d.InReplyTo).Scan(&state); err != nil {
				return notFound(err)
			}
			if !state.Valid() || state.Terminal() {
				return teams.ErrConflict
			}
			if err = activePairs(ctx, conn, d.Route.RunID, original.Route.Sender, original.Recipient); err != nil {
				return err
			}
			_, err = conn.ExecContext(ctx, `UPDATE team_host_delegations SET state=? WHERE delivery_key=?`, mesh.TaskCompleted, d.InReplyTo)
			if err != nil {
				return err
			}
		} else {
			if d.From != d.Route.Sender.Actor || d.Verb != d.Route.Verb {
				return teams.ErrConflict
			}
			// The library authorized this immutable retained route. A non-reply
			// send may finish after later roster removal; never retarget it.
		}
		_, err = conn.ExecContext(ctx, `INSERT INTO team_host_deliveries(delivery_key,payload) VALUES(?,?)`, d.IdempotencyKey, payload)
		if err != nil {
			return err
		}
		if d.Verb == mesh.Delegate {
			_, err = conn.ExecContext(ctx, `INSERT INTO team_host_delegations(delivery_key,state) VALUES(?,?)`, d.IdempotencyKey, mesh.TaskWorking)
		}
		return err
	})
}

// FlushMessages retries a bounded page of accepted outbox entries. Ports retain
// keyed receipts after side effects; a crash before dispatched=1 simply retries
// the same key, content, policy and session. All entries in the page are attempted.
func (h *Host) FlushMessages(ctx context.Context, limit int) error {
	if limit < 1 {
		return errors.New("delivery flush: positive limit required")
	}
	entries, err := h.recoveryPage(ctx, "deliveries", limit)
	if err != nil {
		return err
	}
	var failures []error
	for _, e := range entries {
		d, err := h.GetDelivery(ctx, e.key)
		if err == nil {
			err = h.retainedSessionEnded(ctx, d)
			if err == nil {
				err = h.ports.Messenger.Deliver(ctx, d)
			}
		}
		failures = append(failures, err, h.finishAttempt(ctx, "deliveries", e.key, err))
	}
	return errors.Join(failures...)
}

// Cleanup ownership is retained after retirement; never ask a transport to
// deliver to a session the host already stopped successfully.
func (h *Host) retainedSessionEnded(ctx context.Context, d teams.Delivery) error {
	var stopped bool
	err := h.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM team_host_intents WHERE tombstone<>'' AND cleaned=1 AND json_extract(member,'$.actor')=? AND json_extract(member,'$.session_id')=?)`, d.Recipient.Actor, d.Recipient.SessionID).Scan(&stopped)
	if err != nil {
		return err
	}
	if stopped {
		return ErrSessionGone
	}
	return nil
}
