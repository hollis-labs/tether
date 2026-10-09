//go:build !windows

package store

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/hollis-labs/tether/internal/shimhost"
	"golang.org/x/sys/unix"
)

// PrepareCodexTeamReplacementTx admits no diagnostic or serialized proof. The
// original receipt/actor/session locks and retained host authority are required
// independently of the same-Store historical accounting capability.
func (s *Store) PrepareCodexTeamReplacementTx(ctx context.Context, q ReplacementTx, in TeamReplacementInput, proof *CodexReplacementAccounting, canonical shimhost.Receipt) (SessionReplacement, error) {
	return s.prepareTeamReplacementTx(ctx, q, in, codexReplacementProof{store: s, proof: proof, receipt: canonical})
}

type codexReplacementProof struct {
	store   *Store
	proof   *CodexReplacementAccounting
	receipt shimhost.Receipt
}

func (p codexReplacementProof) validate(ctx context.Context, q ReplacementTx, in TeamReplacementInput) (replacementCustodyEvidence, error) {
	var evidence replacementCustodyEvidence
	if p.proof == nil || !p.receipt.Retired || p.receipt.Session != in.Source.ID || in.Plan.ProviderBrand != "codex" || !replacementPIDAbsent(p.receipt.HostPID) || !replacementPIDAbsent(p.receipt.ProviderPID) {
		return evidence, ErrSessionReplacementUnavailable
	}
	if err := p.store.ValidateCodexReplacementAccountingTx(ctx, q, p.proof, p.receipt); err != nil {
		return evidence, err
	}
	ref := p.proof.Reference()
	if ref.SessionID != in.Source.ID || ref.ShimKey != p.receipt.OperationKey || ref.Revision == 0 || ref.StateSHA256 == "" || ref.CustodySHA256 == "" || ref.JournalSHA256 == "" || ref.JournalHighWater == "" || ref.PublicAcceptanceSHA256 == "" || ref.MessagingSHA256 == "" {
		return evidence, ErrSessionReplacementUnavailable
	}
	custody, err := json.Marshal(p.receipt)
	if err != nil {
		return evidence, err
	}
	reference, err := json.Marshal(ref)
	if err != nil {
		return evidence, err
	}
	evidence = replacementCustodyEvidence{revision: strconv.FormatUint(ref.Revision, 10), digest: ref.StateSHA256, custody: string(custody), reference: string(reference)}
	return evidence, ctx.Err()
}

// An ESRCH observation never signals a process. Live/reused identities,
// permission errors and missing PIDs all preserve old custody.
func replacementPIDAbsent(pid int) bool {
	return pid > 0 && errors.Is(unix.Kill(pid, 0), unix.ESRCH)
}
