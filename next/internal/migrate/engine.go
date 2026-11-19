package migrate

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"
	"github.com/hollis-labs/libs/mesh/core/messaging/delivery"

	"github.com/hollis-labs/tether/next/internal/store"
)

// Phase is a migrate-from run's phase (plan section 7.3).
type Phase string

const (
	// PhaseBulk copies before the window; the database never serves after it alone.
	PhaseBulk Phase = "bulk"
	// PhaseFinal syncs every table by key range against the stopped source and, when verification
	// passes, marks the database complete.
	PhaseFinal Phase = "final"
)

// Mark states (tether_migration_mark.state). The S0 guard (internal/legacyguard) admits only complete.
const (
	MarkRunning  = "running"
	MarkBulk     = "bulk"
	MarkComplete = "complete"
)

// FamilyS12a is the first table family (CW-20261115-0035): the class A tables and messages. Later
// families are added as their domain slices land.
var FamilyS12a = []string{
	"event_identity", "event_retention_policies", "event_retention_admission", "execution_policy",
	"fabric_actors", "fabric_agents", "fabric_definition_artifacts", "fabric_definition_revisions",
	"logical_agents", "pairing_grants", "principals", "settings", "team_definitions",
	"workers", "worker_mail_grants",
	"messages",
}

// FamilyS12b is the class B family (CW-20261115-0042): events, the identity audit and the telemetry tables.
// The event-identity seed and high-water mark are class A (FamilyS12a); events' sqlite_sequence is carried
// with events.
var FamilyS12b = []string{
	"events", "identity_audit", "proxy_events", "event_ingest_digests", "retention_audit",
	"turn_inputs", "session_usage", "team_activation_evidence",
}

// FamilyS12c is the first class C family (CW-20261115-0090): the messaging tables, go-messaging's delivery
// library tables, and sessions with their satellites, i.e. the tables whose store code is in S2b. messages
// itself is in FamilyS12a.
var FamilyS12c = []string{
	// messaging
	"broker_envelopes", "channel_publications", "message_aliases", "address_aliases",
	"message_event_checkpoints", "message_event_deferred", "message_event_journal", "message_purge_audit",
	"recipient_wake_admissions", "routing_replies", "seat_wake_clock", "wake_triage", "a2a_tasks",
	// go-messaging's delivery library (SQLite only: the library has no Postgres store yet)
	"messaging_delivery_schema", "messaging_messages", "messaging_deliveries", "messaging_attempts",
	"messaging_receipts", "messaging_transitions", "messaging_idempotency",
	// environment request and work state, workstreams (#597)
	"environment_request_status", "environment_request_resolutions", "environment_request_turn_ends",
	"environment_work_state", "workstreams", "workstream_projects",
	// sessions and their satellites
	"session_groups", "sessions", "session_idempotency", "session_mcp_policy", "session_provider_mappings",
	"session_refs", "session_replacements", "session_shims", "client_attachments", "launch_plans",
	"checkpoints", "agent_recovery_cursors", "runtime_bindings", "workspace_scratch_allocations",
	"worker_permission_parser", "seat_status",
}

// Families are the table families migrate-from knows, by name, in copy order. "all" is every one of them.
var Families = map[string][]string{
	"a":   FamilyS12a,
	"b":   FamilyS12b,
	"c":   FamilyS12c,
	"all": append(append(append([]string{}, FamilyS12a...), FamilyS12b...), FamilyS12c...),
}

// libraryTable reports whether a table belongs to go-messaging's delivery library: the library creates it
// (delivery.ApplySQLiteSchema), and has no Postgres store yet (Substrate gap CW-20261115-0013).
func libraryTable(t string) bool { return strings.HasPrefix(t, "messaging_") }

// libraryCursor reports whether a table holds positions in go-messaging's transitions: it can only be
// copied together with the library's tables.
func libraryCursor(t string) bool {
	return t == "message_event_checkpoints" || t == "message_event_deferred"
}

// Options configure a run.
type Options struct {
	Phase  Phase
	Tables []string // in copy order; Families["all"] when empty
	Batch  int      // rows per key range; 2000 when 0
	// SourceCheck is the check run on the source before the copy: "full" (PRAGMA integrity_check, the
	// default), "quick" (PRAGMA quick_check: no index cross-check) or "none" (the copy was checked when it
	// was made, as the nightly backup is). It is reported either way.
	SourceCheck string
	// Workers is the number of parallel range workers for tables ranged by an integer key; the number of
	// CPUs, at most 8, when 0.
	Workers int
	// TargetCheck is the check run on the target after the copy: "full" (PRAGMA integrity_check, the
	// default), "quick" (quick_check) or "none" (the range digests already prove the content). The window
	// run can use quick or none when the bulk copy had the full check.
	TargetCheck string
	// Progress, when set, receives one line per table.
	Progress func(format string, args ...any)
}

// ErrIncomplete means a final run would leave source data behind: it is refused, never marked complete.
var ErrIncomplete = errors.New("a final run must copy every source table")

// ErrTargetComplete means the target's migration is complete (it may have served); migrate-from never rewrites
// it. Use the verify-only mode to re-check it.
var ErrTargetComplete = errors.New("the target database's migration is complete: it is never rewritten (use --verify to re-check it)")

// Report is the result of a run, kept with the cutover record (JSON).
type Report struct {
	Phase           Phase               `json:"phase"`
	SourceSHA256    string              `json:"source_sha256"`
	SourceUnchanged bool                `json:"source_unchanged"`
	Copy            []TableReport       `json:"copy"`
	Verify          []TableReport       `json:"verify"`
	Counts          map[string][2]int64 `json:"counts"`                 // source, target
	Sequences       map[string][2]int64 `json:"sequences,omitempty"`    // source, target (SQLite)
	ForeignKeys     map[string][2]int64 `json:"foreign_keys,omitempty"` // violations: source, target (SQLite)
	// ForeignKeysDeferred counts target violations whose parent table is in a family not copied yet.
	ForeignKeysDeferred map[string]int64 `json:"foreign_keys_deferred,omitempty"`
	// HashSeconds is the time spent hashing the source, before its first read and after its last.
	HashSeconds float64 `json:"hash_seconds"`
	Integrity   string  `json:"integrity,omitempty"`
	// VerifyMode is "incremental" after a run (only the ranges the copy wrote are rechecked; the copy's
	// own digests proved the rest) or "full" (Verify: every range walked).
	VerifyMode string `json:"verify_mode"`
	// Uncovered lists the source tables in no family and on no exclusion list (a final run refuses them).
	Uncovered []string `json:"uncovered,omitempty"`
	// Skipped lists the tables of the run that were not copied, with why (go-messaging's tables on a
	// Postgres target).
	Skipped []string `json:"skipped,omitempty"`
	// TargetCheck names the check run on the target after the copy (Options.TargetCheck); Integrity is
	// its result.
	TargetCheck        string  `json:"target_check,omitempty"`
	TargetCheckSeconds float64 `json:"target_check_seconds,omitempty"`
	// SourceWALHeader is true for a source with a WAL-mode header (a .backup copy of a WAL database).
	SourceWALHeader bool `json:"source_wal_header"`
	// SourceCheck names the check run on the source (Options.SourceCheck); SourceIntegrity is its result.
	SourceCheck string `json:"source_check"`
	// SourceIntegrity is the source check's result, before the copy starts.
	SourceIntegrity        string  `json:"source_integrity,omitempty"`
	SourceIntegritySeconds float64 `json:"source_integrity_seconds,omitempty"`
	// Conversions counts values whose storage class the target changes (Postgres only: text in a bytea
	// column, bytes in a text column), per table.column. On SQLite every value keeps its class.
	Conversions   map[string]int64   `json:"conversions,omitempty"`
	MessagesSeq   string             `json:"messages_seq"`
	Problems      []string           `json:"problems,omitempty"`
	Mark          string             `json:"mark"`
	CopySeconds   float64            `json:"copy_seconds"`
	VerifySeconds float64            `json:"verify_seconds"`
	RowsPerSecond map[string]float64 `json:"rows_per_second"`
}

// OK is true when the run verified with no problem.
func (r *Report) OK() bool { return len(r.Problems) == 0 }

// ErrLiveSource means the source is, or may be, a live database (or a raw copy of one missing its WAL).
var ErrLiveSource = errors.New("migrate-from needs a copy of the old database, not the live file")

// CheckSource resolves path (symlinks included) and refuses anything that may be live or incomplete: a
// -wal, -shm or -journal file beside the resolved file (a process has it open, or a write was cut short),
// or a file that is not SQLite. The source is then opened read-only and immutable and never checkpointed.
//
// Use VACUUM INTO for the copy (the nightly backup does; it is the rule for the live database). A .backup
// copy is accepted too: it keeps a WAL-mode header with no -wal file, and is complete. A WAL-mode source
// must pass the source integrity check (Run refuses SourceCheck "none" for it), since a raw copy of a WAL
// database's main file without its -wal could miss committed pages that no header shows.
//
// It returns the resolved path, which the run then reads and hashes.
func CheckSource(path string) (string, error) {
	if path == "" {
		return "", errors.New("--old-db is required")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	if resolved, err = filepath.Abs(resolved); err != nil {
		return "", err
	}
	for _, side := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(resolved + side); err == nil {
			return "", fmt.Errorf("%w: %s has a %s file beside it", ErrLiveSource, resolved, side)
		}
	}
	f, err := os.Open(resolved) //nolint:gosec // the operator names the source copy
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	var hdr [100]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return "", fmt.Errorf("read source header: %w", err)
	}
	if string(hdr[:16]) != "SQLite format 3\x00" {
		return "", fmt.Errorf("%s is not a SQLite database", resolved)
	}
	return resolved, nil
}

// walHeader reports whether the database file at path has a WAL-mode header (bytes 18 and 19 are 2).
func walHeader(path string) (bool, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the source copy
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	var hdr [20]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return false, err
	}
	return hdr[18] == 2 || hdr[19] == 2, nil
}

// OpenSource opens a copy of an old database read-only and immutable: nothing is written to it, not
// even a lock or a WAL index. Pass the path CheckSource returned. Never pass the live file: run
// migrate-from on a copy (plan section 7.2).
func OpenSource(path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?mode=ro&immutable=1")
	if err != nil {
		return nil, err
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open source: %w", err)
	}
	if n == 0 {
		_ = db.Close()
		return nil, errors.New("open source: not an old Tether database (no schema_migrations table)")
	}
	return db, nil
}

// FileSHA256 hashes a file.
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // the operator names the source copy
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

// Engine copies an old database into the new store and proves the copy.
type Engine struct {
	src     *sql.DB
	srcPath string
	raw     *gostore.Store // the target store, for the engine's DDL (triggers, marks, sequences)
	stores  *store.Stores  // row writes go through the class handles
	// conversions is the current run's Report.Conversions, filled by the walkers under convMu (ranged
	// tables are walked by parallel workers).
	conversions map[string]int64
	convMu      sync.Mutex
	// runID stamps this run's stored range digests and its mark.
	runID string
	// workers is the number of parallel range workers for ranged tables.
	workers int
	// stored holds the target's range digests the previous verified run left (loaded at the start of a
	// run, then cleared from the database); fresh holds this run's, written back when it verifies.
	stored, fresh map[string]map[int64]string
}

// New returns an engine from the source copy at srcPath (opened by OpenSource) to the target store.
func New(src *sql.DB, srcPath string, target *gostore.Store) *Engine {
	return &Engine{src: src, srcPath: srcPath, raw: target, stores: store.New(target)}
}

// Run copies (bulk) or syncs (final) the tables, then verifies them and writes the marks.
func (e *Engine) Run(ctx context.Context, opt Options) (*Report, error) {
	if opt.Phase != PhaseBulk && opt.Phase != PhaseFinal {
		return nil, fmt.Errorf("phase must be %s or %s", PhaseBulk, PhaseFinal)
	}
	tables := opt.Tables
	if len(tables) == 0 {
		tables = Families["all"]
	}
	batch := opt.Batch
	if batch <= 0 {
		batch = 2000
	}
	progress := opt.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	rep := &Report{Phase: opt.Phase, Counts: map[string][2]int64{}, RowsPerSecond: map[string]float64{}, Conversions: map[string]int64{}}
	e.conversions = rep.Conversions
	// A final run marks the database complete, and a complete database is never rewritten: whatever it
	// leaves out never arrives. So it must cover every source table (each in the run, or on the committed
	// exclusion list with its reason), and every one of the run's tables must be supported by the target.
	if opt.Phase == PhaseFinal {
		if !coversAll(tables, true) {
			return nil, fmt.Errorf("%w: a final run must copy every family (--family all)", ErrIncomplete)
		}
		missing, err := e.uncovered(ctx, tables)
		if err != nil {
			return nil, err
		}
		if len(missing) > 0 {
			rep.Uncovered = missing
			return rep, fmt.Errorf("%w: %d source table(s) are in no family and on no exclusion list: %s", ErrIncomplete, len(missing), joinTables(missing))
		}
		if !e.sqlite() {
			return rep, fmt.Errorf("%w: go-messaging has no Postgres store (CW-20261115-0013), so its tables and the delivery checkpoint cannot be copied to a Postgres target", ErrIncomplete)
		}
	}
	switch opt.TargetCheck {
	case "", "full", "quick", "none":
	default:
		return nil, fmt.Errorf("target check must be full, quick or none, not %q", opt.TargetCheck)
	}
	// The source is hashed before its first read and after its last: a change between the two shows. (A
	// hash running alongside the reads could miss a byte changed after it was read but before the hash
	// reached it.)
	hashStart := time.Now()
	sum, err := FileSHA256(e.srcPath)
	if err != nil {
		return nil, fmt.Errorf("hash source: %w", err)
	}
	rep.HashSeconds += time.Since(hashStart).Seconds()
	rep.SourceSHA256 = sum
	if err := e.ensureBookkeeping(ctx); err != nil {
		return nil, err
	}
	// A completed target may already have served: a sync would delete the rows written since and revert
	// the ones updated. It is never rewritten; re-check it with Verify, which writes nothing.
	prevState, prevRun, err := e.markState(ctx)
	switch {
	case err != nil:
		return nil, err
	case prevState == MarkComplete:
		return nil, ErrTargetComplete
	}
	wal, err := walHeader(e.srcPath)
	if err != nil {
		return nil, err
	}
	rep.SourceWALHeader = wal
	if wal && opt.SourceCheck == "none" {
		return nil, fmt.Errorf("%w: %s has a WAL-mode header (a .backup copy, or a raw copy that may miss its -wal): it needs the source check (full or quick), not none", ErrLiveSource, e.srcPath)
	}
	integrityStart := time.Now()
	var pragma string
	switch opt.SourceCheck {
	case "", "full":
		pragma, rep.SourceCheck = `PRAGMA integrity_check`, "integrity_check"
	case "quick":
		pragma, rep.SourceCheck = `PRAGMA quick_check`, "quick_check"
	case "none":
		rep.SourceCheck = "none"
	default:
		return nil, fmt.Errorf("source check must be full, quick or none, not %q", opt.SourceCheck)
	}
	if pragma != "" {
		if err := e.src.QueryRowContext(ctx, pragma).Scan(&rep.SourceIntegrity); err != nil {
			return nil, fmt.Errorf("source %s: %w", rep.SourceCheck, err)
		}
		if rep.SourceIntegrity != "ok" {
			return nil, fmt.Errorf("source %s: %s", rep.SourceCheck, rep.SourceIntegrity)
		}
	}
	rep.SourceIntegritySeconds = time.Since(integrityStart).Seconds()
	e.runID = fmt.Sprintf("%d", time.Now().UnixNano())
	if err := e.setMark(ctx, MarkRunning, opt.Phase, sum, false); err != nil {
		return nil, err
	}
	e.workers = workerCount(opt.Workers)
	// The stored range digests describe the target as the last verified run left it. They are trusted
	// only if that run left the mark at bulk with its run id (no serve can open a bulk database: the guard
	// refuses it, so only migrate-from has written it since), with the same range width, digest format and
	// target schema version. They are taken into memory and cleared now, so a run that stops part way
	// leaves none behind to trust.
	if err := e.takeStoredDigests(ctx, prevState == MarkBulk, prevRun, int64(batch)); err != nil {
		return nil, err
	}
	e.fresh = map[string]map[int64]string{}
	if tables, err = e.prepareLibrary(ctx, tables, rep, true); err != nil {
		return nil, err
	}
	specs := make([]tableSpec, 0, len(tables))
	for _, t := range tables {
		sp, err := specFor(ctx, e.src, e.raw, t)
		if err != nil {
			return nil, err
		}
		if _, ok := store.ClassOf(t); !ok {
			return nil, fmt.Errorf("%s has no storage class", t)
		}
		if err := e.checkKeys(ctx, sp); err != nil {
			return nil, err
		}
		specs = append(specs, sp)
	}

	// Copy with the target's triggers off: a copied row is history, not a new event. Inserting messages
	// must not write journal rows, and principals must not be re-checked against today's sessions.
	if err := e.disableTriggers(ctx, tables); err != nil {
		return nil, err
	}
	started := time.Now()
	for _, sp := range specs {
		tr, err := e.walk(ctx, sp, batch, true)
		if err != nil {
			_ = e.enableTriggers(context.WithoutCancel(ctx))
			return nil, err
		}
		rep.Copy = append(rep.Copy, tr)
		if tr.Seconds > 0 {
			rep.RowsPerSecond[sp.name] = float64(tr.Rows) / tr.Seconds
		}
		progress("copy %-30s %9d rows %5d ranges %5d written %7.2fs", sp.name, tr.Rows, tr.Ranges, tr.Written, tr.Seconds)
	}
	if err := e.carrySequences(ctx, tables); err != nil {
		_ = e.enableTriggers(context.WithoutCancel(ctx))
		return nil, err
	}
	// Re-enabled even if ctx was canceled during the copy (a Ctrl-C must not leave triggers off).
	if err := e.enableTriggers(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	rep.CopySeconds = time.Since(started).Seconds()

	started = time.Now()
	if err := e.verify(ctx, specs, batch, rep, progress, rep.Copy, opt.TargetCheck); err != nil {
		return nil, err
	}
	rep.VerifySeconds = time.Since(started).Seconds()
	hashStart = time.Now()
	after, err := FileSHA256(e.srcPath)
	if err != nil {
		return nil, fmt.Errorf("hash source: %w", err)
	}
	rep.HashSeconds += time.Since(hashStart).Seconds()
	rep.SourceUnchanged = after == sum
	if !rep.SourceUnchanged {
		rep.Problems = append(rep.Problems, "the source changed during the run")
	}
	state := MarkBulk
	if opt.Phase == PhaseFinal {
		state = MarkComplete
	}
	if !rep.OK() {
		state = MarkRunning
	}
	if err := e.recordTables(ctx, opt.Phase, sum, rep); err != nil {
		return nil, err
	}
	if rep.OK() {
		if err := e.storeDigests(ctx, int64(batch)); err != nil {
			return nil, err
		}
	}
	if err := e.setMark(ctx, state, opt.Phase, sum, state != MarkRunning); err != nil {
		return nil, err
	}
	rep.Mark = state
	return rep, nil
}

func (e *Engine) walk(ctx context.Context, sp tableSpec, batch int, sync bool) (TableReport, error) {
	if sp.ranged() {
		rw := &rangedWalker{walker: e.walker(sp, batch, sync), width: int64(batch), workers: e.workers}
		if sync {
			rw.stored = e.stored[sp.name]
		}
		tr, digests, err := rw.run(ctx)
		if err == nil && sync {
			e.fresh[sp.name] = digests
		}
		return tr, err
	}
	return e.walker(sp, batch, sync).run(ctx)
}

func (e *Engine) walker(sp tableSpec, batch int, sync bool) *walker {
	c, _ := store.ClassOf(sp.name)
	h := map[store.Class]*store.Handle{store.A: e.stores.A(), store.B: e.stores.B(), store.C: e.stores.C()}[c]
	return &walker{src: e.src, tgt: h, sp: sp, batch: batch, sync: sync, pg: e.raw.Dialect().Name() == gostore.DriverPostgres, conversions: e.conversions, convMu: &e.convMu}
}

// Verify compares the target with the source without changing either and without touching the marks:
// the check an operator runs on a migrated database at any time.
func (e *Engine) Verify(ctx context.Context, opt Options) (*Report, error) {
	tables := opt.Tables
	if len(tables) == 0 {
		tables = Families["all"]
	}
	batch := opt.Batch
	if batch <= 0 {
		batch = 2000
	}
	progress := opt.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	rep := &Report{Phase: opt.Phase, Counts: map[string][2]int64{}, RowsPerSecond: map[string]float64{}, Conversions: map[string]int64{}}
	e.conversions = rep.Conversions
	sum, err := FileSHA256(e.srcPath)
	if err != nil {
		return nil, err
	}
	rep.SourceSHA256, rep.SourceUnchanged = sum, true
	if tables, err = e.prepareLibrary(ctx, tables, rep, false); err != nil {
		return nil, err
	}
	var specs []tableSpec
	for _, t := range tables {
		sp, err := specFor(ctx, e.src, e.raw, t)
		if err != nil {
			return nil, err
		}
		specs = append(specs, sp)
	}
	e.workers = workerCount(opt.Workers)
	started := time.Now()
	if err := e.verify(ctx, specs, batch, rep, progress, nil, opt.TargetCheck); err != nil {
		return nil, err
	}
	rep.VerifySeconds = time.Since(started).Seconds()
	return rep, nil
}

// markState reads the target's mark state and the id of the run that set it ("" when there is none).
func (e *Engine) markState(ctx context.Context) (string, string, error) {
	var state string
	var run sql.NullString
	err := e.raw.Reader().QueryRowContext(ctx, `SELECT state, run_id FROM tether_migration_mark WHERE id = 1`).Scan(&state, &run)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return state, run.String, err
}

// coversAll reports whether tables holds every table of every family (the library tables only where the
// target can hold them).
func coversAll(tables []string, library bool) bool {
	have := map[string]bool{}
	for _, t := range tables {
		have[t] = true
	}
	for _, t := range Families["all"] {
		if !have[t] && (library || !libraryTable(t)) {
			return false
		}
	}
	return true
}

// prepareLibrary makes the target ready for go-messaging's tables: on SQLite it applies the library's own
// schema through the sanctioned library handle, so the tables exist (with the library's shape) before they
// are copied. On Postgres the library has no store, so its tables are dropped from the run and reported.
func (e *Engine) prepareLibrary(ctx context.Context, tables []string, rep *Report, apply bool) ([]string, error) {
	needs := false
	for _, t := range tables {
		needs = needs || libraryTable(t)
	}
	if !needs {
		return tables, nil
	}
	// The old store creates go-messaging's tables at open, so a live database always has them; a source
	// that never opened the library has none, and nothing of the library's to copy.
	var present []string
	for _, t := range tables {
		if libraryTable(t) {
			var n int
			if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, t).Scan(&n); err != nil {
				return nil, err
			}
			if n == 0 {
				rep.Skipped = append(rep.Skipped, t+": not in the source (the old store never opened go-messaging)")
				continue
			}
		}
		present = append(present, t)
	}
	tables = present
	if !e.sqlite() {
		// The library has no Postgres store. The delivery event checkpoint and the deferred transitions
		// refer to its transitions, so they go with it: a checkpoint copied without its transitions would
		// skip the first N once the library's own ids start at 1.
		var kept []string
		for _, t := range tables {
			if libraryTable(t) || libraryCursor(t) {
				rep.Skipped = append(rep.Skipped, t+": go-messaging has no Postgres store (CW-20261115-0013)")
				continue
			}
			kept = append(kept, t)
		}
		return kept, nil
	}
	if !apply {
		// Verify writes nothing: a library table the target lacks is reported, not created.
		var kept []string
		for _, t := range tables {
			if libraryTable(t) {
				var n int
				if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, t).Scan(&n); err != nil {
					return nil, err
				}
				if n == 0 {
					rep.Skipped = append(rep.Skipped, t+": not in the target (verify creates nothing)")
					continue
				}
			}
			kept = append(kept, t)
		}
		return kept, nil
	}
	// Applied once, on a target that does not have the library's tables yet. The library's schema step
	// writes its own version row; after the first run that row is the copied source's, and applying the
	// schema again would rewrite it, so every rerun would see that range differ.
	var n int
	if err := e.raw.Reader().QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'messaging_delivery_schema'`).Scan(&n); err != nil {
		return nil, err
	}
	if n > 0 {
		return tables, nil
	}
	db, err := e.stores.LibraryDB(ctx)
	if err != nil {
		return nil, err
	}
	if err := delivery.ApplySQLiteSchema(ctx, db); err != nil {
		return nil, fmt.Errorf("apply go-messaging's schema to the target: %w", err)
	}
	return tables, nil
}

// ErrNullKey means a source row has a NULL in its primary key (SQLite allows it in a non-INTEGER key). Such a
// row never falls in a key range, so it is refused up front, with where it is, rather than missed.
var ErrNullKey = errors.New("source rows with a NULL primary key")

func (e *Engine) checkKeys(ctx context.Context, sp tableSpec) error {
	conds := make([]string, len(sp.key))
	for i, k := range sp.key {
		conds[i] = quote(k) + ` IS NULL`
	}
	var n int64
	//nolint:gosec // names come from the schema
	if err := e.src.QueryRowContext(ctx, `SELECT count(*) FROM `+quote(sp.name)+` WHERE `+strings.Join(conds, " OR ")).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("%w: %d in %s (key %s); a key decision is needed before this table copies", ErrNullKey, n, sp.name, strings.Join(sp.key, ", "))
	}
	return nil
}

func workerCount(n int) int {
	if n > 0 {
		return n
	}
	return min(runtime.NumCPU(), 8)
}
