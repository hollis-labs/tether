package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"time"
)

// Conversation-to-session mapping (CW-20261118-0008, docs/conversations.md).
//
// A conversation is durable for a seat. The daemon mints its ID when the first
// session for that seat is written, and every later session written for the
// seat (a fresh launch, a relaunch/resume, a team replacement) moves the row
// to the new session and raises generation by exactly 1, in the transaction
// that writes the session. Nothing here calls the chat host: the resolver runs
// under its lease lock.

// ConversationOwnerOperator is the owner of every conversation this daemon
// mints today: the daemon serves one operator.
const ConversationOwnerOperator = "msg://user/agent-mux/operator"

// ConversationKindStable is a conversation durable for one seat.
const ConversationKindStable = "stable"

// Reasons a conversation reached a generation (conversation_generations.reason).
const (
	ConversationReasonCreate  = "create"  // first session for the seat: generation 1
	ConversationReasonLaunch  = "launch"  // a fresh session for a seat that already had one
	ConversationReasonResume  = "resume"  // intent=resume (relaunch or checkpoint resume)
	ConversationReasonReplace = "replace" // team replacement of a lost execution
)

// ErrConversationNotFound is returned when no conversation matches.
var ErrConversationNotFound = errors.New("store: conversation not found")

// Conversation is the current state of one conversations row.
type Conversation struct {
	ID         string
	Owner      string
	Agent      string
	Seat       string
	Kind       string
	Generation int64
	SessionID  string
	AttemptID  string
	// WorkerID is the worker the session runs on; empty means this daemon.
	WorkerID  string
	CreatedAt string
	UpdatedAt string
}

// conversationSeat names the conversation a session belongs to. agent is the
// catalog agent (operative); seat is the logical agent the session runs as.
// They differ only for team members, whose logical id is per-member.
type conversationSeat struct {
	owner, agent, seat string
}

// seatForSession returns the seat a session row speaks for, or ok=false when
// the session has no logical agent (an ad-hoc session has no conversation).
func seatForSession(logicalAgentID, catalogAgentID string) (conversationSeat, bool) {
	if logicalAgentID == "" {
		return conversationSeat{}, false
	}
	agent := catalogAgentID
	if agent == "" {
		agent = logicalAgentID
	}
	return conversationSeat{owner: ConversationOwnerOperator, agent: agent, seat: logicalAgentID}, true
}

// recordConversationTx points the seat's conversation at sessionID inside the
// caller's transaction q, which has already inserted the session row. The
// first session for a seat creates the row at generation 1; any other session
// moves it and raises generation by exactly 1. Writing the session that is
// already current is a no-op, so a retried write never bumps twice.
func recordConversationTx(ctx context.Context, q ReplacementTx, sessionID, catalogAgentID, reason string) error {
	var logicalAgentID, workerID string
	err := q.QueryRowContext(ctx, `SELECT COALESCE(logical_agent_id,''), COALESCE(worker_id,'') FROM sessions WHERE id=?`, sessionID).Scan(&logicalAgentID, &workerID)
	if err != nil {
		return fmt.Errorf("conversation: read session %s: %w", sessionID, err)
	}
	seat, ok := seatForSession(logicalAgentID, catalogAgentID)
	if !ok {
		return nil
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	attempt := newULID(time.Now())
	var id, current string
	var generation int64
	err = q.QueryRowContext(ctx, `SELECT conversation_id, generation, session_id FROM conversations WHERE owner=? AND agent=? AND seat=? AND kind=?`,
		seat.owner, seat.agent, seat.seat, ConversationKindStable).Scan(&id, &generation, &current)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		id, generation = newULID(time.Now()), 1
		if _, err = q.ExecContext(ctx, `INSERT INTO conversations(conversation_id,owner,agent,seat,kind,generation,session_id,attempt_id,worker_id,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			id, seat.owner, seat.agent, seat.seat, ConversationKindStable, generation, sessionID, attempt, workerID, now, now); err != nil {
			return fmt.Errorf("conversation: create: %w", err)
		}
		reason = ConversationReasonCreate
	case err != nil:
		return fmt.Errorf("conversation: read seat: %w", err)
	case current == sessionID:
		return nil
	default:
		// Compare-and-set on the generation read above: the bump is exactly 1.
		res, err := q.ExecContext(ctx, `UPDATE conversations SET generation=generation+1, session_id=?, attempt_id=?, worker_id=?, updated_at=? WHERE conversation_id=? AND generation=?`,
			sessionID, attempt, workerID, now, id, generation)
		if err != nil {
			return fmt.Errorf("conversation: move: %w", err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("conversation: move: %w", err)
		}
		if n != 1 {
			return fmt.Errorf("conversation: move %s: generation %d changed underneath", id, generation)
		}
		generation++
	}
	if _, err = q.ExecContext(ctx, `INSERT INTO conversation_generations(conversation_id,generation,session_id,attempt_id,worker_id,reason,created_at) VALUES(?,?,?,?,?,?,?)`,
		id, generation, sessionID, attempt, workerID, reason, now); err != nil {
		return fmt.Errorf("conversation: record generation: %w", err)
	}
	return nil
}

// sessionConversationReason classifies a freshly inserted session row.
func sessionConversationReason(intent string) string {
	if intent == "resume" {
		return ConversationReasonResume
	}
	return ConversationReasonLaunch
}

const conversationColumns = `conversation_id,owner,agent,seat,kind,generation,session_id,attempt_id,worker_id,created_at,updated_at`

func scanConversation(row *sql.Row) (Conversation, error) {
	var c Conversation
	err := row.Scan(&c.ID, &c.Owner, &c.Agent, &c.Seat, &c.Kind, &c.Generation, &c.SessionID, &c.AttemptID, &c.WorkerID, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrConversationNotFound
	}
	return c, err
}

// Conversation returns the current state of conversation id.
func (s *Store) Conversation(ctx context.Context, id string) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE conversation_id=?`, id))
}

// ConversationForSession returns the current state of the conversation that
// sessionID has ever belonged to, and the generation sessionID held. When
// sessionID has been superseded, the returned Conversation names the newer
// session and a higher generation; the caller decides what stale means.
func (s *Store) ConversationForSession(ctx context.Context, sessionID string) (Conversation, int64, error) {
	var id string
	var held int64
	err := s.db.QueryRowContext(ctx, `SELECT conversation_id, generation FROM conversation_generations WHERE session_id=? ORDER BY generation DESC LIMIT 1`, sessionID).Scan(&id, &held)
	if errors.Is(err, sql.ErrNoRows) {
		return Conversation{}, 0, ErrConversationNotFound
	}
	if err != nil {
		return Conversation{}, 0, err
	}
	c, err := s.Conversation(ctx, id)
	return c, held, err
}

// SeatConversation returns the stable conversation for a seat, if one exists.
func (s *Store) SeatConversation(ctx context.Context, owner, agent, seat string) (Conversation, error) {
	return scanConversation(s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE owner=? AND agent=? AND seat=? AND kind=?`, owner, agent, seat, ConversationKindStable))
}

// crockford is the ULID alphabet (Crockford base32).
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newULID returns a ULID: 48 bits of Unix milliseconds then 80 random bits,
// as 26 Crockford base32 characters. It is opaque to every reader.
func newULID(t time.Time) string {
	var b [16]byte
	var ts [8]byte
	binary.BigEndian.PutUint64(ts[:], uint64(t.UnixMilli())) //nolint:gosec // G115: wall-clock ms after 1970 is positive
	copy(b[:6], ts[2:])
	if _, err := rand.Read(b[6:]); err != nil {
		panic(fmt.Sprintf("store: ulid entropy: %v", err))
	}
	// 128 bits encode as 130 with two leading zero bits.
	var out [26]byte
	for i := range out {
		var v byte
		for j := 0; j < 5; j++ {
			bit := i*5 + j - 2
			v <<= 1
			if bit >= 0 && b[bit/8]&(0x80>>(bit%8)) != 0 {
				v |= 1
			}
		}
		out[i] = crockford[v]
	}
	return string(out[:])
}
