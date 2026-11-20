package catalogapply

import "fmt"

// ApprovalItem binds an approval to one exact plan.
type ApprovalItem struct {
	ID     string `json:"id"`
	Digest string `json:"digest"`
}

// BatchApprovalRequest shares one credential and explicit acknowledgements.
// Batch approval always requires the operator credential; single-plan
// delegation does not extend to this operation.
type BatchApprovalRequest struct {
	Plans         []ApprovalItem
	Passphrase    string
	AckPrivileged bool
	AllowRunning  bool
}

// ApprovalResult reports independent per-plan outcomes; failed plans are unchanged.
type ApprovalResult struct {
	ID    string `json:"id"`
	Plan  *Plan  `json:"plan,omitempty"`
	Error string `json:"error,omitempty"`
}

// ApproveBatch verifies the operator credential once, so each incorrect batch
// counts once against lockout, regardless of batch length. No public bypass of
// the credential check exists; all plan checks are shared with Approve.
func (e *Engine) ApproveBatch(req BatchApprovalRequest) ([]ApprovalResult, error) {
	if len(req.Plans) == 0 || len(req.Plans) > maxPlans {
		return nil, fmt.Errorf("batch must contain 1-%d plans", maxPlans)
	}
	seen := map[string]bool{}
	for _, p := range req.Plans {
		if p.ID == "" || seen[p.ID] {
			return nil, fmt.Errorf("batch has empty or duplicate plan id %q", p.ID)
		}
		seen[p.ID] = true
	}
	if e.Approver == nil {
		return nil, ErrNoPassphrase
	}
	if err := e.Approver.Verify(req.Passphrase); err != nil {
		return nil, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	results := make([]ApprovalResult, 0, len(req.Plans))
	for _, item := range req.Plans {
		p, err := e.approveLocked(ApprovalRequest{PlanID: item.ID, Digest: item.Digest,
			AckPrivileged: req.AckPrivileged, AllowRunning: req.AllowRunning}, true)
		result := ApprovalResult{ID: item.ID, Plan: p}
		if err != nil {
			result.Error = err.Error()
		}
		results = append(results, result)
	}
	return results, nil
}
