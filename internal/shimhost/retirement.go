//go:build !windows

package shimhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if readRetirementJSON(metadataPath(r), shim.MaxFrame, &saved) != nil || !samePlacement(saved, r) || saved.Retired || !saved.Attempted || len(saved.SubmissionAttemptID) != 64 || saved.SubmissionAttemptID != r.SubmissionAttemptID || saved.OperationKey != r.OperationKey || saved.Fingerprint != r.Fingerprint {
		return fail("outcome_unknown", "submission intent changed; retain placement")
	}
	return nil
}

// Control-file reads refuse nonregular files before reading; O_NONBLOCK avoids
// waiting on a substituted FIFO while the placement lock is held.
func readRetirementJSON(path string, limit int64, value any) error {
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
	return json.Unmarshal(b, value)
}

// PlacementFence holds the canonical placement lock until Close. Its durable
// fence survives Close/crashes; it is not removed on refusal or unsupported proof.
type PlacementFence struct {
	lock    *os.File
	receipt Receipt
	fence   SubmissionFence
}

func (f *PlacementFence) Receipt() Receipt          { return f.receipt }
func (f *PlacementFence) Evidence() SubmissionFence { return f.fence }
func (f *PlacementFence) Close() error              { return f.lock.Close() }

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
	if err = readRetirementJSON(metadataPath(expected), shim.MaxFrame, &saved); err != nil {
		return nil, fail("outcome_unknown", "canonical retirement receipt unavailable")
	}
	if !samePlacement(saved, r) || saved.OperationKey != r.OperationKey || saved.Fingerprint != r.Fingerprint || saved.SubmissionAttemptID != r.SubmissionAttemptID || !saved.Attempted {
		return nil, fail("identity_mismatch", "retirement receipt changed")
	}
	raw, err := json.Marshal(saved)
	if err != nil {
		return nil, fail("outcome_unknown", "receipt revision unavailable")
	}
	digest := sha256.Sum256(raw)
	fence := SubmissionFence{Version: submissionFenceVersion, RetirementOperation: operation, Session: saved.Session, Instance: saved.Instance, Generation: saved.Generation, PlacementOperation: saved.OperationKey, SubmissionAttemptID: saved.SubmissionAttemptID, ReceiptRevision: hex.EncodeToString(digest[:])}
	path := filepath.Join(dir, "retirement-fence.json")
	var old SubmissionFence
	if err = readRetirementJSON(path, shim.MaxFrame, &old); err == nil {
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
	return &PlacementFence{lock: lock, receipt: saved, fence: fence}, nil
}

// ProofCapability deliberately refuses current incomplete production lineage.
// A persisted submit fence or an absent unit is not complete owned absence.
func (f *PlacementFence) ProofCapability() error {
	return fail("unsupported", "complete submission drain and descendant containment proof unavailable")
}
