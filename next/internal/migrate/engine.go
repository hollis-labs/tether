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
	"strings"
	"time"

	gostore "github.com/hollis-labs/libs/core/store"

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

// Options configure a run.
type Options struct {
	Phase  Phase
	Tables []string // in copy order; FamilyS12a when empty
	Batch  int      // rows per key range; 2000 when 0
	// SourceCheck is the check run on the source before the copy: "full" (PRAGMA integrity_check, the
	// default), "quick" (PRAGMA quick_check: no index cross-check) or "none" (the copy was checked when it
	// was made, as the nightly backup is). It is reported either way.
	SourceCheck string
	// Progress, when set, receives one line per table.
	Progress func(format string, args ...any)
}

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
	// HashSeconds is the time spent hashing the source, before and after; CopySeconds and VerifySeconds exclude it.
	HashSeconds float64 `json:"hash_seconds"`
	Integrity   string  `json:"integrity,omitempty"`
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

// CheckSource resolves path (symlinks included) and refuses anything that may be live or incomplete:
//   - a -wal, -shm or -journal file beside the resolved file: a process has it open, or a write was cut
//     short;
//   - a WAL-mode header (bytes 18 and 19 are 2): a WAL database's committed pages may live in its -wal
//     file, so a raw copy of the main file alone can silently miss them, and immutable=1 would not
//     notice. A copy made with VACUUM INTO (the nightly backup) or .backup is in rollback mode.
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
	if hdr[18] == 2 || hdr[19] == 2 {
		return "", fmt.Errorf("%w: %s is in WAL mode; take the copy with VACUUM INTO or .backup (rollback mode), never by copying the file", ErrLiveSource, resolved)
	}
	return resolved, nil
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
	// conversions is the current run's Report.Conversions, filled by the walkers.
	conversions map[string]int64
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
		tables = FamilyS12a
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
	switch state, err := e.markState(ctx); {
	case err != nil:
		return nil, err
	case state == MarkComplete:
		return nil, ErrTargetComplete
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
	if err := e.setMark(ctx, MarkRunning, opt.Phase, sum, false); err != nil {
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
	if err := e.verify(ctx, specs, batch, rep, progress); err != nil {
		return nil, err
	}
	rep.VerifySeconds = time.Since(started).Seconds() // the source hash after the run is in HashSeconds
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
	if err := e.setMark(ctx, state, opt.Phase, sum, state != MarkRunning); err != nil {
		return nil, err
	}
	rep.Mark = state
	return rep, nil
}

func (e *Engine) walk(ctx context.Context, sp tableSpec, batch int, sync bool) (TableReport, error) {
	c, _ := store.ClassOf(sp.name)
	h := map[store.Class]*store.Handle{store.A: e.stores.A(), store.B: e.stores.B(), store.C: e.stores.C()}[c]
	w := &walker{src: e.src, tgt: h, sp: sp, batch: batch, sync: sync, pg: e.raw.Dialect().Name() == gostore.DriverPostgres, conversions: e.conversions}
	return w.run(ctx)
}

// Verify compares the target with the source without changing either and without touching the marks:
// the check an operator runs on a migrated database at any time.
func (e *Engine) Verify(ctx context.Context, opt Options) (*Report, error) {
	tables := opt.Tables
	if len(tables) == 0 {
		tables = FamilyS12a
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
	var specs []tableSpec
	for _, t := range tables {
		sp, err := specFor(ctx, e.src, e.raw, t)
		if err != nil {
			return nil, err
		}
		specs = append(specs, sp)
	}
	started := time.Now()
	if err := e.verify(ctx, specs, batch, rep, progress); err != nil {
		return nil, err
	}
	rep.VerifySeconds = time.Since(started).Seconds()
	return rep, nil
}

// markState reads the target's mark state ("" when there is none).
func (e *Engine) markState(ctx context.Context) (string, error) {
	var state string
	err := e.raw.Reader().QueryRowContext(ctx, `SELECT state FROM tether_migration_mark WHERE id = 1`).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return state, err
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
