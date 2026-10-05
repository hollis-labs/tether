//go:build !windows

package shimhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/hollis-labs/substrate/harness/shim"
)

const submissionFenceVersion = "shim.submission-fence.v1"

// SubmissionFence is nonsecret, retained control evidence. A fence prevents
// NEW submissions. It cannot establish that an earlier queued job or escaped
// descendant is absent, and it never authorizes cleanup by itself.
type SubmissionFence struct {
	Version, RetirementOperation                             string
	Session, Instance                                        string
	Generation                                               uint64
	PlacementOperation, SubmissionAttemptID, ReceiptRevision string
}

func (p *Provider) refuseFencedSubmission(dir string) error {
	_, err := os.Lstat(filepath.Join(dir, "retirement-fence.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return fail("outcome_unknown", "submission is fenced; retain original placement")
}

// verifySubmissionIntent is the last host-owned check before each actual
// submission call, while Place still holds the original placement lock.
func (p *Provider) verifySubmissionIntent(dir string, r Receipt) error {
	if err := p.refuseFencedSubmission(dir); err != nil {
		return err
	}
	var saved Receipt
	if readRetirementJSON(metadataPath(r), &saved) != nil || !samePlacement(saved, r) || saved.Retired || !saved.Attempted || !validSubmissionAttempt(saved.SubmissionAttemptID) || saved.SubmissionAttemptID != r.SubmissionAttemptID || saved.OperationKey != r.OperationKey || saved.Fingerprint != r.Fingerprint {
		return fail("outcome_unknown", "submission intent changed; retain placement")
	}
	return nil
}

// Control-file reads refuse nonregular files before reading; O_NONBLOCK avoids
// waiting on a substituted FIFO while the placement lock is held.
func readRetirementJSON(path string, value any) error {
	const limit int64 = shim.MaxFrame
	if !filepath.IsAbs(path) || checkPrivateDir(filepath.Dir(path)) != nil {
		return fail("outcome_unknown", "private control root unavailable")
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(fd), path)
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ok || int64(st.Uid) != int64(os.Getuid()) {
		return fail("outcome_unknown", "private control file unavailable")
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) > limit {
		return fail("outcome_unknown", "private control record unreadable")
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, named) {
		return fail("outcome_unknown", "private control identity changed")
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(value); err != nil {
		return fail("outcome_unknown", "private control shape unavailable")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fail("outcome_unknown", "private control shape unavailable")
	}
	return nil
}

// PlacementFence holds the canonical placement lock until Close. Its durable
// fence survives Close/crashes; it is not removed on refusal or unsupported proof.
type PlacementFence struct {
	mu       sync.Mutex
	closed   bool
	lockPath string
	lock     *os.File
	receipt  Receipt
	fence    SubmissionFence
}

func (f *PlacementFence) Receipt() Receipt {
	if f == nil {
		return Receipt{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.receipt
}
func (f *PlacementFence) Evidence() SubmissionFence {
	if f == nil {
		return SubmissionFence{}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fence
}
func (f *PlacementFence) Close() error {
	if f == nil {
		return fail("outcome_unknown", "retirement lease unavailable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.lock == nil {
		return fail("outcome_unknown", "retirement lease unavailable")
	}
	return f.lock.Close()
}

// AcquireRetirementFence accepts trusted canonical placement evidence, never a
// caller's path or absence boolean. Lifecycle/controller exclusion must also be
// held by the composition owner. This seam does not signal any process/unit.
func (p *Provider) AcquireRetirementFence(ctx context.Context, r Receipt, operation string) (*PlacementFence, error) {
	if operation == "" || len(operation) > 256 || strings.ContainsAny(operation, "\x00\r\n") || r.Session == "" || r.Instance == "" || r.Generation == 0 {
		return nil, fail("identity_mismatch", "retirement identity is incomplete")
	}
	expected := p.PlacementIdentity(r.OperationKey, shim.Launch{Session: r.Session, Instance: r.Instance, Generation: r.Generation})
	if !samePlacement(expected, r) {
		return nil, fail("identity_mismatch", "noncanonical retirement placement")
	}
	if !validSubmissionAttempt(r.SubmissionAttemptID) {
		return nil, fail("unsupported", "original submission lineage unavailable")
	}
	dir := p.dir(r.Session)
	lock, err := LockWait(ctx, filepath.Join(dir, "placement.lock"), p.cfg.LockWait)
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = lock.Close()
		}
	}()
	var saved Receipt
	if err = readRetirementJSON(metadataPath(expected), &saved); err != nil {
		return nil, fail("outcome_unknown", "canonical retirement receipt unavailable")
	}
	if !samePlacement(saved, r) || saved.OperationKey != r.OperationKey || saved.Fingerprint != r.Fingerprint || saved.SubmissionAttemptID != r.SubmissionAttemptID || !saved.Attempted {
		return nil, fail("identity_mismatch", "retirement receipt changed")
	}
	// The retained fence binds the original receipt, including its original
	// non-retired state. Only the retirement bit may advance on a bound retry.
	original := saved
	original.Retired = false
	raw, err := json.Marshal(original)
	if err != nil {
		return nil, fail("outcome_unknown", "receipt revision unavailable")
	}
	digest := sha256.Sum256(raw)
	fence := SubmissionFence{Version: submissionFenceVersion, RetirementOperation: operation, Session: saved.Session, Instance: saved.Instance, Generation: saved.Generation, PlacementOperation: saved.OperationKey, SubmissionAttemptID: saved.SubmissionAttemptID, ReceiptRevision: hex.EncodeToString(digest[:])}
	path := filepath.Join(dir, "retirement-fence.json")
	var old SubmissionFence
	if err = readRetirementJSON(path, &old); err == nil {
		if old != fence {
			return nil, fail("identity_mismatch", "retirement fence changed")
		}
	} else if !os.IsNotExist(err) {
		return nil, fail("outcome_unknown", "retirement fence unreadable")
	} else {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err = WritePrivateJSON(path, fence); err != nil {
			return nil, fail("outcome_unknown", "retirement fence commit uncertain")
		}
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	ok = true
	return &PlacementFence{lock: lock, lockPath: filepath.Join(dir, "placement.lock"), receipt: saved, fence: fence}, nil
}

// ProofCapability deliberately refuses current incomplete production lineage.
// A persisted submit fence or an absent unit is not complete owned absence.
func (f *PlacementFence) ProofCapability() error {
	return fail("unsupported", "complete submission drain and descendant containment proof unavailable")
}

// RefreshRetirement reads the canonical receipt while the original placement
// lock remains held. It preserves original descriptor/inventory metadata after
// payload cleanup; it never infers a process outcome from a missing descriptor.
func (f *PlacementFence) RefreshRetirement(ctx context.Context) (Receipt, error) {
	if f == nil {
		return Receipt{}, fail("outcome_unknown", "retirement lease unavailable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.refreshRetirementLocked(ctx)
}
func (f *PlacementFence) refreshRetirementLocked(ctx context.Context) (Receipt, error) {
	if f.closed || f.lock == nil || !f.heldLockIdentity() {
		return Receipt{}, fail("outcome_unknown", "retirement lease custody lost")
	}

	if ctx.Err() != nil {
		return Receipt{}, ctx.Err()
	}
	var saved Receipt
	if readRetirementJSON(metadataPath(f.receipt), &saved) != nil {
		return Receipt{}, fail("outcome_unknown", "retirement receipt unavailable")
	}
	expected := f.receipt
	expected.Retired = saved.Retired
	if expected != saved || (f.receipt.Retired && !saved.Retired) {
		return Receipt{}, fail("identity_mismatch", "retirement receipt changed")
	}
	var fence SubmissionFence
	if readRetirementJSON(filepath.Join(filepath.Dir(metadataPath(saved)), "retirement-fence.json"), &fence) != nil || fence != f.fence {
		return Receipt{}, fail("identity_mismatch", "retirement fence changed")
	}
	if ctx.Err() != nil {
		return Receipt{}, ctx.Err()
	}
	return saved, nil
}

// CommitRetired is a canonical state mutation, not an absence proof. The caller
// must already hold complete lifecycle/controller exclusion and a verified
// positive owned absence observation. Current ProofCapability remains Unsupported.
func (f *PlacementFence) CommitRetired(ctx context.Context) (bool, error) {
	if f == nil {
		return false, fail("outcome_unknown", "retirement lease unavailable")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	saved, err := f.refreshRetirementLocked(ctx)
	if err != nil {
		return false, err
	}
	if saved.Retired {
		return false, nil
	}
	saved.Retired = true
	if ctx.Err() != nil {
		return false, ctx.Err()
	}
	if WritePrivateJSON(metadataPath(saved), saved) != nil {
		return true, fail("outcome_unknown", "retirement receipt commit uncertain")
	}
	f.receipt = saved
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return true, nil
}

func validSubmissionAttempt(id string) bool {
	decoded, err := hex.DecodeString(id)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == id
}

// Legacy teardown cannot clean a placement claimed by the audited retirement
// workflow. Any fence entry, including unreadable/malformed entries, retains
// payloads without signals until the bound durable audit is reconciled.
func (p *Provider) refuseFencedTeardown(dir string) error {
	_, err := os.Lstat(filepath.Join(dir, "retirement-fence.json"))
	if os.IsNotExist(err) {
		return nil
	}
	return fail("outcome_unknown", "fenced retirement requires audited recovery")
}

func (f *PlacementFence) heldLockIdentity() bool {
	held, err := f.lock.Stat()
	if err != nil {
		return false
	}
	named, err := os.Lstat(f.lockPath)
	if err != nil || !held.Mode().IsRegular() || held.Mode().Perm() != 0600 || !os.SameFile(held, named) || checkPrivateDir(filepath.Dir(f.lockPath)) != nil {
		return false
	}
	st, ok := held.Sys().(*syscall.Stat_t)
	return ok && int64(st.Uid) == int64(os.Getuid())
}
