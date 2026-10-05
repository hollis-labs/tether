package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/shimretire"
)

// ShimCleanupAdmission is a closed native SQL/time/filesystem kernel. No
// callback is accepted or invoked inside its exclusion boundary. Its private
// custody is not constructible by a runtime caller or an interface assertion.
type ShimCleanupAdmission struct {
	db      *sql.DB
	custody *cleanupCustody
}

// NewShimCleanupAdmission deliberately cannot mint the currently unavailable
// compound lifecycle/controller/journal/authority capability. Placement locks,
// release functions, sessionLaunchGate and fixture proofs cannot substitute.
func NewShimCleanupAdmission(_ *Store) (*ShimCleanupAdmission, error) {
	return nil, shimretire.ErrCleanupUnsupported
}

// Only an owned native producer could create this capability. Current source
// has no production producer; a private store-package fixture constructor binds
// synthetic authority/containment and real owned temporary native resources.
type cleanupCustody struct {
	gate                             chan struct{}
	root                             *os.Root
	rootIdentity                     shimretire.FileIdentity
	rootID, ownerID, custodyRevision string
	request                          shimretire.Request
	proof                            shimretire.Observation
	sweep                            *shimretire.SweepRequest
	database                         *os.File
	directory                        *os.File
	namespace                        map[string]*os.File
	databasePath                     string
}

type nativeAdmissionClock struct{ anchor time.Time }

func newNativeAdmissionClock() nativeAdmissionClock {
	return nativeAdmissionClock{anchor: time.Now()}
}

func (c nativeAdmissionClock) now() time.Time {
	// Monotonic elapsed survives wall-clock rollback; native wall elapsed also
	// accounts for forward adjustments and suspend on clocks that exclude it.
	elapsed := time.Since(c.anchor)
	wall := time.Now().UTC().Sub(c.anchor.UTC())
	if wall > elapsed {
		elapsed = wall
	}
	if elapsed < 0 {
		elapsed = 0
	}
	return c.anchor.UTC().Add(elapsed)
}

func (a *ShimCleanupAdmission) RemoveOwned(ctx context.Context, attempt shimretire.CleanupAttempt) (mutation shimretire.Mutation, err error) {
	mutation = shimretire.NoChange
	refuse := shimretire.ErrCleanupUnsupported
	if a == nil || a.db == nil || a.custody == nil || ctx == nil {
		return mutation, refuse
	}
	c := a.custody
	clock := newNativeAdmissionClock()
	if attempt.Receipt.Request != c.request || !reflect.DeepEqual(attempt.Proof, c.proof) || attempt.Root != c.root || attempt.RootIdentity != c.rootIdentity || attempt.Artifact.RootID != c.rootID || attempt.Artifact.OwnerID != c.ownerID || attempt.Artifact.CustodyRevision != c.custodyRevision || !validCleanupAttempt(attempt, clock.now()) {
		return mutation, refuse
	}
	attempt.Receipt = attempt.Receipt.Clone()
	attempt.Proof.Contradictions = slices.Clone(attempt.Proof.Contradictions)
	if attempt.Cursor != nil {
		if shimretire.ValidateSweepCursor(*attempt.Cursor) != nil {
			return mutation, refuse
		}
		cursor := attempt.Cursor.Clone()
		attempt.Cursor = &cursor
	}
	remaining := attempt.Proof.ValidUntil.Sub(clock.now())
	if remaining <= 0 {
		return mutation, refuse
	}
	if remaining > 5*time.Second {
		remaining = 5 * time.Second
	}
	if attempt.Cursor != nil && attempt.Cursor.Request.Budget.MaxDuration > 0 && attempt.Cursor.Request.Budget.MaxDuration < remaining {
		remaining = attempt.Cursor.Request.Budget.MaxDuration
	}
	bounded, cancel := context.WithTimeout(ctx, remaining)
	defer cancel()
	// The native compound guard excludes all covered native writers. Current
	// production has no such producer. A wait is always cancellable and bounded.
	select {
	case <-bounded.Done():
		return mutation, refuse
	case <-c.gate:
	}
	defer func() { c.gate <- struct{}{} }()
	if bounded.Err() != nil || !c.namedDatabase() {
		return mutation, refuse
	}
	conn, e := a.reserveAdmission(bounded)
	if e != nil {
		return mutation, refuse
	}
	defer func() {
		if conn.Close() != nil {
			if mutation != shimretire.NoChange {
				mutation = shimretire.Uncertain
			}
			err = errors.New("cleanup connection release uncertain")
		}
	}()
	defer func() {
		// A read-only reservation cannot roll back unlink. Release failure after
		// an attempted effect is uncertainty, and callers retain pending evidence.
		release, stop := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer stop()
		if _, e := conn.ExecContext(release, "ROLLBACK"); e != nil || bounded.Err() != nil {
			if mutation != shimretire.NoChange {
				mutation = shimretire.Uncertain
			}
			err = errors.New("cleanup writer reservation release uncertain")
		}
	}()
	current, e := admissionReceipt(bounded, conn, attempt.Receipt.OperationID)
	if e != nil || !sameCleanupReceipt(current, attempt.Receipt) || !c.namedDatabase() {
		return mutation, refuse
	}
	if attempt.Mode == shimretire.RetentionCleanup {
		cursor, e := admissionCursor(bounded, conn, attempt.Cursor.ID)
		if e != nil || !reflect.DeepEqual(cursor, *attempt.Cursor) || c.sweep == nil || !reflect.DeepEqual(cursor.Request, *c.sweep) || !validRetentionAttempt(attempt, cursor, clock.now()) {
			return mutation, refuse
		}
	} else if c.sweep != nil || attempt.Cursor != nil {
		return mutation, refuse
	}
	// Original freshness, context and physical facts are established by native
	// code after all extensible callbacks/reads. No user hook follows these facts.
	if !validCleanupAttempt(attempt, clock.now()) || bounded.Err() != nil || !c.namedDatabase() || !nativeCleanupRoot(c.root, c.rootIdentity) || !nativeCleanupParents(c.root, attempt.Artifact.RelativePath) {
		return mutation, refuse
	}
	info, e := c.root.Lstat(attempt.Artifact.RelativePath)
	if os.IsNotExist(e) {
		if bounded.Err() != nil || !validCleanupAttempt(attempt, clock.now()) {
			return mutation, refuse
		}
		return mutation, nil
	}
	if e != nil || attempt.Artifact.Size > math.MaxInt64 || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || nativeCleanupIdentity(info) != attempt.Artifact.Identity || info.Size() != int64(attempt.Artifact.Size) {
		return mutation, refuse
	}
	parent, e := c.root.Open(path.Dir(attempt.Artifact.RelativePath))
	if e != nil {
		return mutation, refuse
	}
	defer func() { _ = parent.Close() }()
	// Time is compiled in, not a Clock callback capable of changing the audit.
	// SQL writers and covered native writers remain excluded through the effect.
	if bounded.Err() != nil || !validCleanupAttempt(attempt, clock.now()) {
		return mutation, refuse
	}
	mutation = shimretire.Uncertain // attempted effects never claim rollback
	if c.root.Remove(attempt.Artifact.RelativePath) != nil || parent.Sync() != nil || bounded.Err() != nil || !validCleanupAttempt(attempt, clock.now()) {
		return mutation, errors.New("native cleanup outcome uncertain")
	}
	return shimretire.Changed, nil
}

// reserveAdmission is the same actual reservation used by RemoveOwned; its
// returned native connection remains reserved until the caller rolls it back.
// It accepts no effect/read/clock callback and never changes connection settings.
func (a *ShimCleanupAdmission) reserveAdmission(ctx context.Context) (*sql.Conn, error) {
	conn, err := a.db.Conn(ctx)
	if err != nil {
		return nil, shimretire.ErrCleanupUnsupported
	}
	ok := false
	defer func() {
		if !ok {
			_ = conn.Close()
		}
	}()
	var busy, sync, seq int
	var schema, name string
	var journal string
	if conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy) != nil || busy != 0 || conn.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&sync) != nil || (sync != 2 && sync != 3) || conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal) != nil || journal != "wal" || conn.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &schema, &name) != nil || seq != 0 || schema != "main" || name != a.custody.databasePath || !a.custody.namedDatabase() {
		return nil, shimretire.ErrCleanupUnsupported
	}
	if _, err = conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, shimretire.ErrCleanupUnsupported
	}
	ok = true
	return conn, nil
}

func validCleanupAttempt(a shimretire.CleanupAttempt, now time.Time) bool {
	r := a.Receipt
	if shimretire.ValidateReceipt(r) != nil || r.Revision == "" || !r.Snapshot.Retired || a.Snapshot != r.Snapshot || shimretire.Verify(r.Request, a.Snapshot, a.Proof, now).Outcome != shimretire.Eligible || a.Root == nil {
		return false
	}
	switch a.Mode {
	case shimretire.DescriptorCleanup:
		return a.Cursor == nil && a.Artifact == r.Snapshot.Descriptor && (r.Phase == shimretire.RetirementCommitted || r.Phase == shimretire.DescriptorCleanupComplete || r.Phase == shimretire.StateReconciled)
	case shimretire.RetentionCleanup:
		return a.Cursor != nil && r.Phase == shimretire.StateReconciled && len(r.Obligations) == 0 && slices.Contains(r.Inventory, a.Artifact)
	}
	return false
}

func validRetentionAttempt(a shimretire.CleanupAttempt, c shimretire.SweepCursor, now time.Time) bool {
	if shimretire.ValidateSweepCursor(c) != nil || c.Request.InspectOnly || c.Phase != shimretire.CursorIntent || c.Pending == nil || c.Pending.Artifact != a.Artifact || c.Pending.RetirementOperation != a.Receipt.OperationID || c.Pending.Hold != shimretire.NoRetentionHold || c.Pending.HoldRevision == "" || !slices.Contains(c.Request.Policy.Categories, a.Artifact.Category) || !slices.Contains(c.Request.Policy.RootIDs, a.Artifact.RootID) || a.Artifact.Category == shimretire.Descriptor || a.Artifact.Size > c.Request.Budget.MaxBytesRemoved || a.Artifact.Size > c.Request.Budget.MaxBytesExamined || a.Receipt.RetiredAt.After(now) {
		return false
	}
	if (a.Artifact.Category == shimretire.Journal || a.Artifact.Category == shimretire.Bridge) && c.Request.Policy.ReplayWaiver == "" {
		return false
	}
	// No admitted native producer currently binds current quota totals. A cursor
	// intent is not evidence that a byte/item threshold remains exceeded.
	if c.Request.Policy.MaxRetainedBytes != nil || c.Request.Policy.MaxRetainedItems != nil || c.Request.Policy.MinRetiredAge == nil {
		return false
	}
	return now.Sub(a.Receipt.RetiredAt) >= *c.Request.Policy.MinRetiredAge
}

func sameCleanupReceipt(a, b shimretire.Receipt) bool {
	return reflect.DeepEqual(a, b)
}

func admissionReceipt(ctx context.Context, conn *sql.Conn, id string) (shimretire.Receipt, error) {
	var r shimretire.Receipt
	var raw, revision, digest, session, phase string
	var retired sql.NullString
	e := conn.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(record_json AS BLOB))<=262144 THEN record_json ELSE NULL END,revision,request_digest,session_id,phase,retired_at FROM session_shim_retirements WHERE operation_id=?`, id).Scan(&raw, &revision, &digest, &session, &phase, &retired)
	if e != nil || strictAdmissionJSON(raw, &r) != nil || shimretire.ValidateReceipt(r) != nil || r.OperationID != id || r.Revision != revision || r.RequestDigest != digest || r.Request.Placement.Session != session || string(r.Phase) != phase || !retirementTimeBound(r.RetiredAt, retired) {
		return shimretire.Receipt{}, ErrShimRetirementConflict
	}
	return r, nil
}

func admissionCursor(ctx context.Context, conn *sql.Conn, id string) (shimretire.SweepCursor, error) {
	var c shimretire.SweepCursor
	var raw, digest, revision string
	e := conn.QueryRowContext(ctx, `SELECT CASE WHEN length(CAST(record_json AS BLOB))<=262144 THEN record_json ELSE NULL END,request_digest,revision FROM session_shim_retention_cursors WHERE cursor_id=?`, id).Scan(&raw, &digest, &revision)
	if e != nil || strictAdmissionJSON(raw, &c) != nil || shimretire.ValidateSweepCursor(c) != nil || c.ID != id || c.Revision != revision || c.RequestDigest != digest {
		return shimretire.SweepCursor{}, ErrShimRetirementConflict
	}
	return c, nil
}

func strictAdmissionJSON(raw string, value any) error {
	d := json.NewDecoder(bytes.NewReader([]byte(raw)))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(new(any)) != io.EOF {
		return ErrShimRetirementConflict
	}
	return nil
}

// Native identities are unavailable on unsupported platforms. Reflection only
// reads native Stat metadata; it invokes no caller/interface methods.
func nativeCleanupIdentity(info os.FileInfo) shimretire.FileIdentity {
	if info == nil {
		return shimretire.FileIdentity{}
	}
	v := reflect.ValueOf(info.Sys())
	if v.Kind() != reflect.Pointer || v.IsNil() || v.Elem().Kind() != reflect.Struct {
		return shimretire.FileIdentity{}
	}
	v = v.Elem()
	field := func(name string) (uint64, bool) {
		x := v.FieldByName(name)
		if !x.IsValid() {
			return 0, false
		}
		switch x.Kind() {
		case reflect.Uint, reflect.Uint32, reflect.Uint64:
			return x.Uint(), true
		case reflect.Int, reflect.Int32, reflect.Int64:
			n := x.Int()
			if n < 0 {
				return 0, false
			}
			return uint64(n), true
		default:
			return 0, false
		}
	}
	uid, u := field("Uid")
	dev, d := field("Dev")
	ino, i := field("Ino")
	links, l := field("Nlink")
	nativeUID := os.Getuid()
	if nativeUID < 0 {
		return shimretire.FileIdentity{}
	}
	if !u || !d || !i || !l || uid != uint64(nativeUID) || ino == 0 {
		return shimretire.FileIdentity{}
	}
	kind := "other"
	if info.Mode().IsRegular() && links == 1 {
		kind = "regular"
	} else if info.IsDir() {
		kind = "directory"
	}
	return shimretire.FileIdentity{Device: dev, Inode: ino, Kind: kind}
}

func nativeCleanupRoot(root *os.Root, identity shimretire.FileIdentity) bool {
	if root == nil || identity.Kind != "directory" {
		return false
	}
	held, e := root.Lstat(".")
	named, n := os.Lstat(root.Name())
	canonical, c := filepath.EvalSymlinks(root.Name())
	return e == nil && n == nil && c == nil && filepath.IsAbs(root.Name()) && canonical == root.Name() && held.IsDir() && held.Mode().Perm() == 0700 && nativeCleanupIdentity(held) == identity && os.SameFile(held, named)
}

func nativeCleanupParents(root *os.Root, name string) bool {
	if name == "." || path.Clean(name) != name || strings.HasPrefix(name, "/") || name == ".." || strings.HasPrefix(name, "../") || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	current := "."
	for _, part := range strings.Split(path.Dir(name), "/") {
		if part == "." {
			continue
		}
		current = path.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || nativeCleanupIdentity(info).Kind != "directory" {
			return false
		}
	}
	return true
}

func (c *cleanupCustody) namedDatabase() bool {
	if c.database == nil || c.directory == nil || !filepath.IsAbs(c.databasePath) || len(c.namespace) != 3 || c.namespace[c.databasePath] != c.database || c.namespace[c.databasePath+"-wal"] == nil || c.namespace[c.databasePath+"-shm"] == nil {
		return false
	}
	canonical, e := filepath.EvalSymlinks(c.databasePath)
	if e != nil || canonical != c.databasePath {
		return false
	}
	for name, file := range c.namespace {
		held, e := file.Stat()
		named, n := os.Lstat(name)
		if e != nil || n != nil || !os.SameFile(held, named) || nativeCleanupIdentity(held).Kind != "regular" || named.Mode().Perm() != 0600 {
			return false
		}
	}
	held, e := c.directory.Stat()
	named, n := os.Lstat(filepath.Dir(c.databasePath))
	return e == nil && n == nil && held.IsDir() && named.Mode().Perm() == 0700 && nativeCleanupIdentity(held).Kind == "directory" && os.SameFile(held, named)
}
