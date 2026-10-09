package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/events"
)

// The retry journal is independent of the SQLite connection pool: a stalled
// metadata read or stage must not leave the only output body in daemon memory.
// It follows the selected state DB, including non-default state roots. Reads
// and writes are daemon-only; OpenReadOnly intentionally has no journal path.
func (s *Store) turnOutputRetryDir() (string, error) {
	if s.stateDBPath == "" || s.stateDBPath == ":memory:" {
		return "", errors.New("turn output retry journal requires a writable file-backed state DB")
	}
	return s.stateDBPath + ".turn-output-retries", nil
}

func validOutputRetryID(id string) bool {
	if len(id) != 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Bound replay allocation, not durability: oversized records are preserved for
// an operator rather than rejected before committing their only durable copy.
const maxOutputRetryBytes = 128 * 1024 * 1024

// WriteTurnOutputRetry commits one private, opaque producer record before its
// first DB attempt. Sync both the file and directories before acknowledging it.
func (s *Store) WriteTurnOutputRetry(id string, payload []byte) error {
	if !validOutputRetryID(id) {
		return errors.New("invalid turn output retry record")
	}
	dir, err := s.turnOutputRetryDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // Directory needs owner traversal; files are 0600.
		return err
	}
	if err := syncOutputRetryDir(filepath.Dir(dir)); err != nil {
		return err
	}
	// A unique temporary name prevents concurrent writers sharing a scratch file.
	tmp := filepath.Join(dir, id+".tmp-"+uuid.NewString())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // Selected state DB directory plus validated hex ID and random scratch suffix.
	if err != nil {
		return err
	}
	defer os.Remove(tmp) //nolint:errcheck // only this writer's uncommitted scratch file
	_, writeErr := f.Write(payload)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	// Linking the synced scratch inode atomically publishes without replacing
	// an earlier committed record or extending its retention on replay.
	if err := os.Link(tmp, filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := os.Remove(tmp); err != nil {
		return err
	}
	return syncOutputRetryDir(dir)
}

func syncOutputRetryDir(dir string) error {
	f, err := os.Open(dir) //nolint:gosec // Only the selected state DB journal directory is synced.
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

// PendingTurnOutputRetries pages lexically by output ID. Expired records remain
// available for explicit operator retention/purge, as do expired DB stages.
func (s *Store) PendingTurnOutputRetries(after string, limit int) ([]string, error) {
	dir, err := s.turnOutputRetryDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 128 {
		limit = 128
	}
	var out []string
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validOutputRetryID(id) || id <= after {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, err
		}
		if !info.ModTime().After(time.Now().Add(-RoutingStageRetention)) {
			continue
		}
		out = append(out, id)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

func (s *Store) ReadTurnOutputRetry(id string) ([]byte, error) {
	if !validOutputRetryID(id) {
		return nil, errors.New("invalid turn output retry ID")
	}
	dir, err := s.turnOutputRetryDir()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(filepath.Join(dir, id+".json")) //nolint:gosec // ID is validated hex; the daemon selects the state DB directory.
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck
	payload, err := io.ReadAll(io.LimitReader(f, maxOutputRetryBytes+1))
	if len(payload) > maxOutputRetryBytes {
		return nil, fmt.Errorf("turn output retry %q is oversized", id)
	}
	return payload, err
}

func (s *Store) CompleteTurnOutputRetry(id string) error {
	if !validOutputRetryID(id) {
		return errors.New("invalid turn output retry ID")
	}
	dir, err := s.turnOutputRetryDir()
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, id+".json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncOutputRetryDir(dir)
}

// HasPublishedTurnOutput closes the crash window between durable bus publication
// and journal acknowledgement. Consumers can also use output_id for deduplication.
func (s *Store) HasPublishedTurnOutput(ctx context.Context, sessionID, outputID string) (bool, error) {
	_, found, err := s.PublishedTurnOutput(ctx, sessionID, outputID)
	return found, err
}

// PublishedTurnOutput restores the acceptance identifiers on producer replay.
func (s *Store) PublishedTurnOutput(ctx context.Context, sessionID, outputID string) (events.TurnOutputEvent, bool, error) {
	return publishedTurnOutput(ctx, s.db, sessionID, outputID)
}

func publishedTurnOutput(ctx context.Context, query stagedOutputExecutor, sessionID, outputID string) (events.TurnOutputEvent, bool, error) {
	var payload string
	err := query.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE session_id=? AND kind=? AND CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.output_id') END=? LIMIT 1`, sessionID, events.KindSessionTurnOutput, outputID).Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return events.TurnOutputEvent{}, false, nil
	}
	if err != nil {
		return events.TurnOutputEvent{}, false, err
	}
	var output events.TurnOutputEvent
	if err := json.Unmarshal([]byte(payload), &output); err != nil {
		return output, false, err
	}
	return output, true, nil
}

// VerifyTurnOutputAcceptance runs inside the hosted delivery owner's receipt
// transaction. Producer acceptance alone cannot certify a protocol inbox drain.
// Both staged and already-attached messages are valid; purged bodies are not.
func (s *Store) VerifyTurnOutputAcceptance(ctx context.Context, tx *sql.Tx, sessionID, outputID, messageID, turnID string) error {
	if tx == nil || sessionID == "" || !validOutputRetryID(outputID) || turnID == "" {
		return errors.New("invalid turn output acceptance")
	}
	output, found, err := publishedTurnOutput(ctx, tx, sessionID, outputID)
	if err != nil {
		return err
	}
	if !found || output.SessionID != sessionID || output.TurnID != turnID || output.MessageID != messageID {
		return errors.New("turn output acceptance does not match durable event")
	}
	route, err := SessionRouteTx(ctx, tx, sessionID)
	if err != nil {
		return err
	}
	selected := route != nil && slices.Contains(route.Kinds, string(output.Kind))
	if selected != (messageID != "") {
		return errors.New("turn output acceptance does not match selected route")
	}
	if messageID == "" {
		return nil
	}
	var payload, metadata sql.NullString
	var sender, channel string
	err = tx.QueryRowContext(ctx, `SELECT from_urn,channel,payload,metadata FROM messages WHERE id=?`, messageID).Scan(&sender, &channel, &payload, &metadata)
	if err != nil {
		return err
	}
	if !payload.Valid || sender != "msg://session/local/"+sessionID || (channel != "" && channel != route.Channel) {
		return errors.New("turn output accepted body unavailable")
	}
	var meta map[string]string
	if err := json.Unmarshal([]byte(metadata.String), &meta); err != nil {
		return errors.New("turn output accepted attribution unavailable")
	}
	if meta["output_id"] != outputID || meta["turn_id"] != turnID || meta["session_id"] != sessionID || meta["kind"] != string(output.Kind) || meta["provider_result_id"] != output.ProviderResultID {
		return errors.New("turn output accepted attribution mismatch")
	}
	return nil
}

// VerifyTurnOutputContent additionally binds a native completion source, kind,
// and original body. It preserves the acceptance-only API for receipt owners
// that already enforce these expected-content checks in their own transaction.
func (s *Store) VerifyTurnOutputContent(ctx context.Context, tx *sql.Tx, sessionID, outputID, messageID, turnID, providerResultID, expectedText, expectedKind string) error {
	if err := s.VerifyTurnOutputAcceptance(ctx, tx, sessionID, outputID, messageID, turnID); err != nil {
		return err
	}
	output, found, err := publishedTurnOutput(ctx, tx, sessionID, outputID)
	if err != nil {
		return err
	}
	if !found || output.ProviderResultID != providerResultID || string(output.Kind) != expectedKind || outputID != TurnOutputID(sessionID, turnID, expectedKind, providerResultID, expectedText) {
		return errors.New("turn output accepted native content identity mismatch")
	}
	if messageID == "" {
		excerpt, truncated := events.TurnOutputExcerpt(expectedText)
		if output.Text != excerpt || output.TextTruncated != truncated {
			return errors.New("turn output accepted excerpt mismatch")
		}
		return nil
	}
	var payload, contentType string
	if err := tx.QueryRowContext(ctx, `SELECT payload,content_type FROM messages WHERE id=?`, messageID).Scan(&payload, &contentType); err != nil {
		return err
	}
	var body struct {
		Text string `json:"text"`
	}
	if contentType != "application/json" || json.Unmarshal([]byte(payload), &body) != nil || body.Text != expectedText {
		return errors.New("turn output accepted body mismatch")
	}
	return nil
}

// TurnOutputID binds one native turn/source/kind and full body independently of
// mutable route, attribution and continuity annotations during producer replay.
func TurnOutputID(sessionID, turnID, kind, providerResultID, text string) string {
	identity, _ := json.Marshal([5]string{sessionID, turnID, kind, providerResultID, text})
	digest := sha256.Sum256(identity)
	return hex.EncodeToString(digest[:])
}
