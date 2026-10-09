//go:build !windows

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hollis-labs/substrate/harness/shim"
	"github.com/hollis-labs/substrate/mesh"
	"github.com/hollis-labs/tether/internal/events"
	"github.com/hollis-labs/tether/internal/messaging/channels"
	"github.com/hollis-labs/tether/internal/shimcodex"
	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

// CodexAccountingSQL reads through the replacement owner's existing write
// transaction. Both *sql.Conn and *sql.Tx implement it; validation never opens
// another transaction, writes, stages output, or touches an execution token.
type CodexAccountingSQL interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// CodexAccountingRef contains only stable evidence references. It is data, not
// a replacement, retirement, delivery, process-absence, or enrollment capability.
type CodexAccountingRef struct {
	SessionID, ShimKey                        string
	Revision                                  uint64
	StateSHA256, CustodySHA256, JournalSHA256 string
	JournalHighWater, PublicAcceptanceSHA256  string
}

// CodexReplacementAccounting is issued only after independent historical
// accounting. The caller must separately prove positive loss/owned retirement,
// original membership, pin and unrevoked binding, freeze old writes, and remap
// all references atomically. An empty inbox or diagnostic cannot construct it.
type CodexReplacementAccounting struct {
	issuer         *Store
	state          shimcodex.State
	receipt        shimhost.Receipt
	reference      CodexAccountingRef
	snapshotSHA256 string
}

func (p *CodexReplacementAccounting) Reference() CodexAccountingRef {
	if p == nil {
		return CodexAccountingRef{}
	}
	return p.reference
}

// CodexAccountingFailure reports only a fixed refusal, never native payloads,
// paths, SQL contents, execution credentials, or a caller-provided error body.
type CodexAccountingFailure struct{ Code string }

func (e *CodexAccountingFailure) Error() string     { return "Codex replacement accounting: " + e.Code }
func (e *CodexAccountingFailure) ErrorCode() string { return e.Code }
func accountingRefuse(code string) error            { return &CodexAccountingFailure{Code: code} }

const accountingJournalBytes = 256 << 20
const accountingRecords = 65536
const accountingTurns = 1024
const accountingOutboxPredicate = `from_urn=? AND (to_urn='msg://service/local/turn-output' OR CASE WHEN json_valid(metadata) THEN json_extract(metadata,'$.output_id') END IS NOT NULL)`

// AccountCodexReplacement reads preserved history even for a terminal session
// or an archived Retired receipt. It does not relax the active delivery issuer:
// neither LoadVerifiedCodexDelivery nor CommitDelivery is called or changed.
func (s *Store) AccountCodexReplacement(ctx context.Context, observed shimcodex.State, canonical shimhost.Receipt) (*CodexReplacementAccounting, error) {
	if s == nil || s.db == nil {
		return nil, accountingRefuse("unavailable")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, accountingRefuse("unavailable")
	}
	defer func() { _ = tx.Rollback() }()
	ref, snapshot, err := s.accountCodexHistory(ctx, tx, observed, canonical, tx)
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(observed)
	var frozen shimcodex.State
	if json.Unmarshal(raw, &frozen) != nil {
		return nil, accountingRefuse("snapshot_changed")
	}
	return &CodexReplacementAccounting{issuer: s, state: frozen, receipt: canonical, reference: ref, snapshotSHA256: snapshot}, nil
}

// ValidateCodexReplacementAccountingTx consumes no evidence and mutates no old
// ledger. Revalidation belongs inside the same locked transaction as the
// replacement lineage, old-write freeze and complete reference remap. A proof
// from another Store, changed ledger, receipt, journal or acceptance refuses.
func (s *Store) ValidateCodexReplacementAccountingTx(ctx context.Context, q CodexAccountingSQL, proof *CodexReplacementAccounting, canonical shimhost.Receipt) error {
	if s == nil || q == nil || proof == nil || proof.issuer != s || proof.receipt != canonical {
		return accountingRefuse("proof_mismatch")
	}
	ref, snapshot, err := s.accountCodexHistory(ctx, q, proof.state, canonical, nil)
	if err != nil {
		return err
	}
	if ref != proof.reference || snapshot != proof.snapshotSHA256 {
		return accountingRefuse("proof_changed")
	}
	return nil
}

type accountingFrozenState struct{ state shimcodex.State }

func (s accountingFrozenState) Load(context.Context) (shimcodex.State, error) { return s.state, nil }
func (accountingFrozenState) Commit(context.Context, uint64, shimcodex.State) error {
	return accountingRefuse("mutation_refused")
}

func (s *Store) accountCodexHistory(ctx context.Context, q CodexAccountingSQL, state shimcodex.State, receipt shimhost.Receipt, verify *sql.Tx) (CodexAccountingRef, string, error) {
	var ref CodexAccountingRef
	if ctx.Err() != nil {
		return ref, "", ctx.Err()
	}
	if _, err := shimcodex.Open(ctx, accountingFrozenState{state}, state.Binding, state.Epoch, false, shimcodex.Limits{InboxItems: 1024, InboxBytes: shimcodex.ProjectionBudget}); err != nil {
		return ref, "", accountingRefuse("invalid_state")
	}
	if len(state.Inbox) != 0 || len(state.Partial) != 0 || state.ActiveTurn != "" {
		return ref, "", accountingRefuse("private_pending")
	}
	for _, op := range state.Operations {
		if op.EffectUnknown || op.RPCError != nil || op.Phase != shimcodex.Answered && op.Phase != shimcodex.NotSubmitted {
			return ref, "", accountingRefuse("effect_pending")
		}
	}
	for _, request := range state.ServerRequests {
		if !request.Written {
			return ref, "", accountingRefuse("callback_pending")
		}
	}
	if state.Delivery != nil {
		if state.Delivery.ActiveTurnID != "" || len(state.Delivery.PartialBytes) != 0 || state.Delivery.AcceptedSourceCursor != state.Cursor || state.Delivery.DeliveredHighWater != state.Cursor {
			return ref, "", accountingRefuse("projection_pending")
		}
		for _, turn := range state.Delivery.Turns {
			if turn.Phase == "open" {
				return ref, "", accountingRefuse("projection_pending")
			}
			for _, item := range turn.Items {
				if item.CompletedSourceID == "" {
					return ref, "", accountingRefuse("projection_pending")
				}
			}
		}
	} else if state.Cursor != "" || len(state.Operations) != 0 {
		return ref, "", accountingRefuse("delivery_missing")
	}
	placement, err := scanShim(q.QueryRowContext(ctx, `SELECT `+shimColumns+` FROM session_shims WHERE session_id=?`, state.Binding.Session))
	if err != nil || placement.Runtime != "codex" || placement.ShimKey != state.Binding.Operation || placement.RuntimeGeneration != state.Binding.Generation || placement.JournalID != state.Binding.Journal || placement.DescriptorPath != receipt.DescriptorPath || placement.SocketPath != receipt.SocketPath || placement.HostBackend != receipt.Backend || placement.UnitName != receipt.UnitName || placement.HostPID != receipt.HostPID || placement.ShimPID != receipt.ShimPID || placement.ProviderPID != receipt.ProviderPID {
		return ref, "", accountingRefuse("custody_mismatch")
	}
	if receipt.Session != state.Binding.Session || receipt.Instance != state.Binding.Instance || receipt.OperationKey != state.Binding.Operation || receipt.Generation != state.Binding.Generation || receipt.Journal != state.Binding.Journal || receipt.SubmissionAttemptID != state.Binding.Attempt || receipt.Fingerprint != state.Binding.Fingerprint || !receipt.Attempted || receipt.PlacementFailure != "" || filepath.Base(receipt.DescriptorPath) != "launch.json" {
		return ref, "", accountingRefuse("custody_mismatch")
	}
	var protocol, revision, raw string
	err = q.QueryRowContext(ctx, `SELECT protocol,revision,state_json FROM codex_shim_protocol WHERE session_id=? AND shim_key=? AND length(CAST(state_json AS BLOB))<=?`, placement.SessionID, placement.ShimKey, shimcodex.ProjectionBudget).Scan(&protocol, &revision, &raw)
	frozen, _ := json.Marshal(state)
	var loaded shimcodex.State
	if err != nil || protocol != shimcodex.Version || revision != strconv.FormatUint(state.Revision, 10) || json.Unmarshal([]byte(raw), &loaded) != nil {
		return ref, "", accountingRefuse("snapshot_changed")
	}
	current, _ := json.Marshal(loaded)
	if !bytes.Equal(frozen, current) {
		return ref, "", accountingRefuse("snapshot_changed")
	}
	var sessionState string
	var route sql.NullString
	if q.QueryRowContext(ctx, `SELECT state,route_json FROM sessions WHERE id=?`, placement.SessionID).Scan(&sessionState, &route) != nil {
		return ref, "", accountingRefuse("snapshot_missing")
	}
	canonical, err := accountingReadFile(filepath.Join(filepath.Dir(receipt.DescriptorPath), "placement.json"), 64<<10)
	var actual shimhost.Receipt
	if err != nil || json.Unmarshal(canonical, &actual) != nil || actual != receipt {
		return ref, "", accountingRefuse("custody_changed")
	}
	if err = s.accountingRetryCensus(ctx, state.Binding.Session); err != nil {
		return ref, "", err
	}
	public, err := accountingPublicSnapshot(ctx, q, state.Binding.Session)
	if err != nil {
		return ref, "", err
	}
	history, err := accountingReadJournal(ctx, filepath.Join(filepath.Dir(receipt.DescriptorPath), "j"), state.Binding)
	if err != nil {
		return ref, "", err
	}
	for _, cursor := range []string{state.Cursor, state.ReplayHighWater, placement.LastCommittedCursor} {
		position, e := shimCursorPosition(state.Binding.Journal, cursor)
		if e != nil || position > uint64(len(history.events)) {
			return ref, "", accountingRefuse("journal_gap")
		}
	}
	if err = s.accountingProjectHistory(ctx, q, verify, state, history.events); err != nil {
		return ref, "", err
	}
	ref = CodexAccountingRef{SessionID: placement.SessionID, ShimKey: placement.ShimKey, Revision: state.Revision, StateSHA256: accountingDigest([]byte(raw)), CustodySHA256: accountingDigest(canonical), JournalSHA256: history.digest, JournalHighWater: history.high, PublicAcceptanceSHA256: public}
	snapshot, _ := json.Marshal(struct {
		Placement SessionShimRow
		State     string
		Route     sql.NullString
	}{placement, sessionState, route})
	return ref, accountingDigest(snapshot), ctx.Err()
}

func accountingDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Open every path component without following symlinks. No descriptor, launch
// secret, journal owner lock, recovery/quarantine path or segment is written.
func accountingOpenDir(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, accountingRefuse("history_unavailable")
	}
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, accountingRefuse("history_unavailable")
	}
	for _, part := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		next, e := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		_ = unix.Close(fd)
		if e != nil {
			return nil, accountingRefuse("history_unavailable")
		}
		fd = next
	}
	f := os.NewFile(uintptr(fd), path)
	info, e := f.Stat()
	var stat unix.Stat_t
	if e != nil || info.Mode().Perm()&0077 != 0 || unix.Fstat(fd, &stat) != nil || int64(stat.Uid) != int64(os.Geteuid()) {
		_ = f.Close()
		return nil, accountingRefuse("history_unavailable")
	}
	return f, nil
}
func accountingReadAt(dir *os.File, name string, limit int64) ([]byte, error) {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, accountingRefuse("history_unavailable")
	}
	f := os.NewFile(uintptr(fd), name)
	defer func() { _ = f.Close() }()
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || int64(stat.Uid) != int64(os.Geteuid()) || stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0077 != 0 || stat.Size > limit {
		return nil, accountingRefuse("history_unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	var after unix.Stat_t
	if err != nil || int64(len(raw)) > limit || unix.Fstat(fd, &after) != nil || stat.Dev != after.Dev || stat.Ino != after.Ino || stat.Size != after.Size || stat.Mtim != after.Mtim || stat.Ctim != after.Ctim {
		return nil, accountingRefuse("history_changed")
	}
	return raw, nil
}
func accountingReadFile(path string, limit int64) ([]byte, error) {
	dir, err := accountingOpenDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	return accountingReadAt(dir, filepath.Base(path), limit)
}

type accountingJournal struct {
	events       []mesh.Event
	digest, high string
}

func accountingReadJournal(ctx context.Context, path string, binding shimcodex.Binding) (accountingJournal, error) {
	var result accountingJournal
	dir, err := accountingOpenDir(path)
	if err != nil {
		return result, err
	}
	defer func() { _ = dir.Close() }()
	identity, err := accountingReadAt(dir, "identity.json", 4096)
	var id struct {
		ID         string `json:"id"`
		Session    string `json:"session"`
		Generation uint64 `json:"generation"`
	}
	if err != nil || json.Unmarshal(identity, &id) != nil || id.ID != binding.Journal || id.Session != binding.Session || id.Generation != binding.Generation {
		return result, accountingRefuse("journal_identity")
	}
	names, err := dir.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) || len(names) > 4096 {
		return result, accountingRefuse("history_budget")
	}
	var segments []string
	for _, entry := range names {
		if entry.Name() == "identity.json" || entry.Name() == "owner.lock" {
			continue
		}
		if entry.IsDir() || len(entry.Name()) != 12 || !strings.HasSuffix(entry.Name(), ".seg") {
			return result, accountingRefuse("journal_incomplete")
		}
		segments = append(segments, entry.Name())
	}
	sort.Strings(segments)
	if len(segments) == 0 {
		return result, accountingRefuse("journal_incomplete")
	}
	h := sha256.New()
	accountingHashPart(h, identity)
	total := int64(len(identity))
	for i, name := range segments {
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
		if name != fmt.Sprintf("%08d.seg", i+1) {
			return result, accountingRefuse("journal_gap")
		}
		raw, e := accountingReadAt(dir, name, 2<<20)
		if e != nil {
			return result, e
		}
		total += int64(len(raw))
		if total > accountingJournalBytes {
			return result, accountingRefuse("history_budget")
		}
		accountingHashPart(h, []byte(name))
		accountingHashPart(h, raw)
		if !bytes.HasPrefix(raw, []byte("SHIMLOG1\n")) {
			return result, accountingRefuse("journal_incomplete")
		}
		raw = raw[9:]
		for len(raw) != 0 {
			if len(result.events) >= accountingRecords || len(raw) < 8 {
				return result, accountingRefuse("journal_incomplete")
			}
			n := int(binary.BigEndian.Uint32(raw[:4]))
			if n == 0 || n > shim.MaxFrame || n > len(raw)-8 {
				return result, accountingRefuse("journal_incomplete")
			}
			body := raw[4 : 4+n]
			if crc32.ChecksumIEEE(body) != binary.BigEndian.Uint32(raw[4+n:8+n]) {
				return result, accountingRefuse("journal_incomplete")
			}
			var event mesh.Event
			if json.Unmarshal(body, &event) != nil || event.Validate() != nil || event.SessionID != binding.Session || event.Generation != binding.Generation || event.Truncated || event.SourceSequence != uint64(len(result.events)+1) || event.Cursor != binding.Journal+":"+strconv.Itoa(len(result.events)+1) {
				return result, accountingRefuse("journal_gap")
			}
			result.events = append(result.events, event)
			raw = raw[8+n:]
		}
	}
	result.high = binding.Journal + ":" + strconv.Itoa(len(result.events))
	result.digest = hex.EncodeToString(h.Sum(nil))
	return result, nil
}
func accountingHashPart(h hash.Hash, part []byte) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(part)))
	_, _ = h.Write(n[:])
	_, _ = h.Write(part)
}

// Census includes retained/expired retry records; the normal replay list skips
// those and cannot establish complete accounting. Any unknown record refuses.
func (s *Store) accountingRetryCensus(ctx context.Context, session string) error {
	path, err := s.turnOutputRetryDir()
	if err != nil {
		return accountingRefuse("retry_unavailable")
	}
	dir, err := accountingOpenDir(path)
	if err != nil {
		if _, e := os.Lstat(path); errors.Is(e, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer func() { _ = dir.Close() }()
	names, err := dir.ReadDir(4097)
	if err != nil && !errors.Is(err, io.EOF) || len(names) > 4096 {
		return accountingRefuse("history_budget")
	}
	for _, entry := range names {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") || !validOutputRetryID(strings.TrimSuffix(entry.Name(), ".json")) {
			return accountingRefuse("retry_unknown")
		}
		raw, e := accountingReadAt(dir, entry.Name(), 128<<20)
		var record struct {
			Version int    `json:"version"`
			Session string `json:"session_id"`
		}
		if e != nil || json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.Session == "" {
			return accountingRefuse("retry_unknown")
		}
		if record.Session == session {
			return accountingRefuse("public_pending")
		}
	}
	return nil
}

// Bind every old public acceptance and outbox row, including extra/unmapped
// output. The caller cannot validate a proof against an altered public ledger.
func accountingPublicSnapshot(ctx context.Context, q CodexAccountingSQL, session string) (string, error) {
	h := sha256.New()
	bytesRead := 0
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT CAST(id AS TEXT),CASE WHEN length(CAST(coalesce(payload_json,'') AS BLOB))<=65536 THEN coalesce(payload_json,'') END FROM events WHERE session_id=? AND kind=? ORDER BY id LIMIT 1025`, []any{session, events.KindSessionTurnOutput}},
		{`SELECT id,CASE WHEN length(CAST(coalesce(payload,'') AS BLOB))<=67108864 THEN coalesce(payload,'') END,content_type,CASE WHEN length(CAST(metadata AS BLOB))<=65536 THEN metadata END,channel,to_urn,coalesce(thread_id,''),kind,CAST(routing_staged AS TEXT) FROM messages WHERE ` + accountingOutboxPredicate + ` ORDER BY id LIMIT 1025`, []any{"msg://session/local/" + session}},
		{`SELECT CAST(p.seq AS TEXT),p.name,p.message_id FROM channel_publications p JOIN messages m ON m.id=p.message_id WHERE m.` + accountingOutboxPredicate + ` ORDER BY p.seq LIMIT 1025`, []any{"msg://session/local/" + session}},
	}
	for _, query := range queries {
		rows, err := q.QueryContext(ctx, query.sql, query.args...)
		if err != nil {
			return "", accountingRefuse("public_unavailable")
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			return "", accountingRefuse("public_unavailable")
		}
		count := 0
		for rows.Next() {
			count++
			if count > accountingTurns {
				_ = rows.Close()
				return "", accountingRefuse("history_budget")
			}
			values := make([]string, len(columns))
			dest := make([]any, len(columns))
			for i := range values {
				dest[i] = &values[i]
			}
			if rows.Scan(dest...) != nil {
				_ = rows.Close()
				return "", accountingRefuse("public_unavailable")
			}
			raw, _ := json.Marshal(values)
			bytesRead += len(raw)
			if bytesRead > 128<<20 {
				_ = rows.Close()
				return "", accountingRefuse("history_budget")
			}
			accountingHashPart(h, raw)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return "", accountingRefuse("public_unavailable")
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type accountingInject struct {
	fingerprint, cursor string
	id                  uint64
	written             bool
}

// Reinterpret the complete journal with the same closed native projector used
// by delivery. Compaction here is local scratch state after existing public
// acceptance has been checked; it never creates or writes a delivery receipt.
func (s *Store) accountingProjectHistory(ctx context.Context, q CodexAccountingSQL, verify *sql.Tx, state shimcodex.State, journal []mesh.Event) error {
	p := shimcodex.Projection{Version: shimcodex.ProjectionVersion, Binding: state.Binding, JournalIdentity: state.Binding.Journal, ProtocolRevision: state.Revision}
	var carry []byte
	var offset, start uint64
	ops := map[uint64]shimcodex.Operation{}
	for _, op := range state.Operations {
		ops[op.ID] = op
	}
	injects := map[string]accountingInject{}
	responses := map[uint64]bool{}
	callbacks := map[string]bool{}
	accepted := map[string]bool{}
	messages := map[string]bool{}
	lastTerminal := ""
	launchSeen, started, exitSeen := false, false, false
	for _, event := range journal {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var sources []shimcodex.Event
		switch event.Kind {
		case "shim.launch_intent":
			var intent struct {
				Instance string `json:"instance"`
			}
			if launchSeen || json.Unmarshal(event.Payload, &intent) != nil || intent.Instance != state.Binding.Instance {
				return accountingRefuse("effect_history_unknown")
			}
			launchSeen = true
		case "shim.started":
			if !launchSeen || started {
				return accountingRefuse("effect_history_unknown")
			}
			started = true
		case "shim.inject_intent":
			var intent struct {
				Key         string `json:"key"`
				Fingerprint string `json:"fingerprint"`
				Mode        string `json:"mode"`
				Delivery    string `json:"delivery"`
			}
			if json.Unmarshal(event.Payload, &intent) != nil || intent.Mode != "input" || intent.Delivery != "immediate" || intent.Fingerprint == "" || !strings.HasPrefix(intent.Key, "codex-") {
				return accountingRefuse("effect_history_unknown")
			}
			id, e := strconv.ParseUint(strings.TrimPrefix(intent.Key, "codex-"), 10, 64)
			op, ok := ops[id]
			if e != nil || intent.Key != "codex-"+strconv.FormatUint(id, 10) || !ok || op.Phase != shimcodex.Answered {
				return accountingRefuse("effect_history_unknown")
			}
			if _, exists := injects[intent.Key]; exists {
				return accountingRefuse("effect_history_unknown")
			}
			injects[intent.Key] = accountingInject{fingerprint: intent.Fingerprint, cursor: event.Cursor, id: id}
		case "shim.inject_outcome":
			var outcome struct {
				Key     string `json:"key"`
				Receipt struct {
					Fingerprint string `json:"fingerprint"`
					Code        string `json:"code"`
					Cursor      string `json:"cursor"`
					Bytes       int    `json:"bytes"`
				} `json:"receipt"`
			}
			if json.Unmarshal(event.Payload, &outcome) != nil {
				return accountingRefuse("effect_history_unknown")
			}
			intent, ok := injects[outcome.Key]
			if !ok || intent.written || outcome.Receipt.Fingerprint != intent.fingerprint || outcome.Receipt.Cursor != intent.cursor || outcome.Receipt.Code != "bytes_written" || outcome.Receipt.Bytes <= 0 {
				return accountingRefuse("effect_pending")
			}
			intent.written = true
			injects[outcome.Key] = intent
		case "shim.control_intent", "shim.control_outcome", "shim.inject_retry":
			// Signal/control completion and duplicate transport effects need their
			// own independently authenticated disposition; never guess from Gone.
			return accountingRefuse("effect_history_unknown")
		case "shim.output":
			if !started || exitSeen {
				return accountingRefuse("journal_gap")
			}
			var output struct {
				Stream   string `json:"stream"`
				Encoding string `json:"encoding"`
				Data     string `json:"data"`
			}
			if json.Unmarshal(event.Payload, &output) != nil || output.Stream != "stdout" || output.Encoding != "base64" {
				return accountingRefuse("output_unknown")
			}
			data, err := base64.StdEncoding.DecodeString(output.Data)
			if err != nil || len(data) > shim.OutputChunk || uint64(len(data)) > ^uint64(0)-offset {
				return accountingRefuse("output_unknown")
			}
			oldOffset := offset
			offset += uint64(len(data))
			carry = append(carry, data...)
			for {
				i := bytes.IndexByte(carry, '\n')
				if i < 0 {
					break
				}
				if i > shimcodex.ProjectionFrameBytes {
					return accountingRefuse("history_budget")
				}
				end := start + uint64(i) + 1
				source := shimcodex.Event{Identity: state.Binding.Journal + ":stdout:" + strconv.FormatUint(start, 10) + ":" + strconv.FormatUint(end, 10), Cursor: event.Cursor, Raw: append([]byte(nil), carry[:i]...)}
				message, e := shimcodex.Decode(source.Raw)
				if e != nil {
					return accountingRefuse("output_unknown")
				}
				if message.Method == "" {
					id, e := strconv.ParseUint(string(message.ID), 10, 64)
					op, ok := ops[id]
					if e != nil || !ok || op.Notification || op.Phase != shimcodex.Answered || message.Error != nil || !accountingJSONEqual(op.Result, message.Result) || responses[id] {
						return accountingRefuse("effect_history_unknown")
					}
					responses[id] = true
				} else if len(message.ID) != 0 {
					matched := false
					for _, request := range state.ServerRequests {
						if request.Source == source.Identity && request.Written && request.Method == message.Method && accountingJSONEqual(request.ID, message.ID) && accountingJSONEqual(request.Params, message.Params) {
							matched = true
							callbacks[request.Source] = true
							break
						}
					}
					if !matched {
						return accountingRefuse("callback_pending")
					}
				}
				sources = append(sources, source)
				carry = carry[i+1:]
				start = end
			}
			if len(carry) > shimcodex.ProjectionFrameBytes {
				return accountingRefuse("history_budget")
			}
			if len(sources) == 0 {
				fragment, _ := json.Marshal(struct {
					Start uint64 `json:"start,string"`
					End   uint64 `json:"end,string"`
					SHA   string `json:"sha256"`
				}{oldOffset, offset, accountingDigest(data)})
				raw, _ := json.Marshal(struct {
					Kind    string          `json:"kind"`
					Payload json.RawMessage `json:"payload"`
				}{"codex.stdout_fragment", fragment})
				sources = []shimcodex.Event{{Identity: state.Binding.Journal + ":" + event.Cursor + ":codex.stdout_fragment", Cursor: event.Cursor, Raw: raw}}
			}
		case "shim.exit":
			var exit shim.Exit
			if exitSeen || state.Exit == nil || event.Cursor != state.ExitCursor || json.Unmarshal(event.Payload, &exit) != nil || exit != *state.Exit {
				return accountingRefuse("exit_unknown")
			}
			exitSeen = true
			sources = []shimcodex.Event{{Identity: state.Binding.Journal + ":exit:" + event.Cursor, Cursor: event.Cursor, Raw: event.Payload}}
		case "shim.pin_adopted", "shim.attached", "shim.detached", "shim.refused":
		default:
			return accountingRefuse("journal_unknown")
		}
		if len(sources) == 0 {
			raw, _ := json.Marshal(struct {
				Kind    string          `json:"kind"`
				Payload json.RawMessage `json:"payload"`
			}{event.Kind, event.Payload})
			sources = []shimcodex.Event{{Identity: state.Binding.Journal + ":" + event.Cursor + ":" + event.Kind, Cursor: event.Cursor, Raw: raw}}
		}
		trial := state
		trial.Inbox = sources
		trial.Delivery = &p
		trial.StreamOffset = offset
		trial.PartialStart = start
		trial.Partial = carry
		trial.ReplayHighWater = ""
		next, err := shimcodex.BuildDeliveryProjection(trial)
		if err != nil {
			return accountingRefuse("projection_unknown")
		}
		for i := range next.Turns {
			turn := &next.Turns[i]
			if turn.Phase == "open" {
				continue
			}
			for _, item := range turn.Items {
				if item.CompletedSourceID == "" {
					return accountingRefuse("projection_pending")
				}
			}
			id := TurnOutputID(state.Binding.Session, turn.StableOutputTurnID, codexTurnKind(*turn), turn.CompletionSourceID, shimcodex.ProjectedTurnText(*turn))
			if !accepted[id] {
				if len(accepted) >= accountingTurns {
					return accountingRefuse("history_budget")
				}
				var payload string
				if q.QueryRowContext(ctx, `SELECT payload_json FROM events WHERE session_id=? AND kind=? AND CASE WHEN json_valid(payload_json) THEN json_extract(payload_json,'$.output_id') END=? LIMIT 1`, state.Binding.Session, events.KindSessionTurnOutput, id).Scan(&payload) != nil {
					return accountingRefuse("public_missing")
				}
				var output events.TurnOutputEvent
				if json.Unmarshal([]byte(payload), &output) != nil || output.OutputID != id || output.SessionID != state.Binding.Session || output.TurnID != turn.StableOutputTurnID || output.ProviderResultID != turn.CompletionSourceID || string(output.Kind) != codexTurnKind(*turn) || output.StopReason != turn.StopReason || output.Runtime != "codex" {
					return accountingRefuse("public_mismatch")
				}
				turn.OutputAcceptanceID = id
				if output.MessageID != "" {
					turn.OutboxMessageIDs = []string{output.MessageID}
					var staged int
					if q.QueryRowContext(ctx, `SELECT routing_staged FROM messages WHERE id=? AND payload IS NOT NULL`, output.MessageID).Scan(&staged) != nil || staged != 0 {
						return accountingRefuse("public_pending")
					}
					var name, to, thread string
					if q.QueryRowContext(ctx, `SELECT p.name,m.to_urn,m.thread_id FROM channel_publications p JOIN messages m ON m.id=p.message_id WHERE p.message_id=?`, output.MessageID).Scan(&name, &to, &thread) != nil {
						return accountingRefuse("public_pending")
					}
					target, e := channels.ChannelAddress(name)
					if e != nil || to != target.URN() || thread != state.Binding.Session {
						return accountingRefuse("public_mismatch")
					}
					messages[output.MessageID] = true
				}
				if verify != nil && s.verifyCodexTurnTx(ctx, verify, state.Binding.Session, *turn) != nil {
					return accountingRefuse("public_mismatch")
				}
				accepted[id] = true
			}
			lastTerminal = turn.NativeTurnID
		}
		// This cursor belongs only to an ephemeral independently checked
		// historical candidate. It is never committed or exported as a receipt.
		next.DeliveredHighWater = event.Cursor
		p = next
	}
	if !launchSeen || !started || len(carry) != 0 || offset != state.StreamOffset || start != state.PartialStart || p.ActiveTurnID != "" || p.ActiveThreadID != "" && p.ActiveThreadID != state.ThreadID || lastTerminal != state.LastTerminal || (state.Exit != nil) != exitSeen {
		return accountingRefuse("history_incomplete")
	}
	for _, intent := range injects {
		if !intent.written {
			return accountingRefuse("effect_pending")
		}
	}
	for _, op := range state.Operations {
		intent, ok := injects["codex-"+strconv.FormatUint(op.ID, 10)]
		if op.Phase == shimcodex.Answered && (!ok || !intent.written || !op.Notification && !responses[op.ID]) || op.Phase == shimcodex.NotSubmitted && ok {
			return accountingRefuse("effect_history_unknown")
		}
	}
	for _, request := range state.ServerRequests {
		if !callbacks[request.Source] {
			return accountingRefuse("callback_pending")
		}
	}
	var outputCount, messageCount int
	if q.QueryRowContext(ctx, `SELECT count(*) FROM events WHERE session_id=? AND kind=?`, state.Binding.Session, events.KindSessionTurnOutput).Scan(&outputCount) != nil || q.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE `+accountingOutboxPredicate, "msg://session/local/"+state.Binding.Session).Scan(&messageCount) != nil {
		return accountingRefuse("public_unavailable")
	}
	if outputCount != len(accepted) || messageCount != len(messages) {
		return accountingRefuse("public_unaccounted")
	}
	return nil
}

func accountingJSONEqual(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	var first, second bytes.Buffer
	return json.Compact(&first, a) == nil && json.Compact(&second, b) == nil && bytes.Equal(first.Bytes(), second.Bytes())
}
