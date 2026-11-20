package catalogapply

// This small local policy is a stopgap: Warden can generate/replace these
// principal bundles without introducing another credential or identity system.
import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const ApproveScope = "catalog.approve"
const maxBreakglass = 4 * time.Hour

type ApprovalRule struct {
	Name    string   `json:"name"`
	Paths   []string `json:"paths"`
	Actions []string `json:"actions"`
}
type PrincipalPolicy struct {
	Scopes     []string       `json:"scopes"`
	Allowlist  []ApprovalRule `json:"allowlist"`
	Privileged bool           `json:"privileged"`
	RateLimit  int            `json:"rate_limit"` // successful approvals per rolling hour
}
type ApproverPolicy struct {
	Principals map[string]PrincipalPolicy `json:"principals"`
	MaxWindow  string                     `json:"max_window,omitempty"`
}
type BreakglassWindow struct {
	Principal      string    `json:"principal"`
	Reason         string    `json:"reason"`
	OpenedAt       time.Time `json:"opened_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	LastSeen       time.Time `json:"last_seen"`
	PolicyDigest   string    `json:"policy_digest"`
	OperatorDigest string    `json:"operator_digest"`
}
type ApproverView struct {
	LegacyDefaults map[string]PrincipalPolicy `json:"legacy_defaults,omitempty"`
	Policy         ApproverPolicy             `json:"policy"`
	Breakglass     *BreakglassWindow          `json:"breakglass,omitempty"`
	LegacyTachyon  bool                       `json:"legacy_tachyon"`
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}
func validApproverURN(s string) bool {
	for _, prefix := range []string{"msg://service/", "msg://agent/"} {
		if strings.HasPrefix(s, prefix) && len(s) > len(prefix) && !strings.ContainsAny(s, "*?[]\\ \t\r\n") {
			return true
		}
	}
	return false
}
func decodeStrict(b []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.Decode(new(any)) != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}
func (e *Engine) policyLocked() (policy ApproverPolicy, legacy bool, policyErr error) {
	defer func() {
		// A parse/read failure must not let an existing window revive when the
		// policy is repaired, including before the first restore in a new process.
		if policyErr != nil && e.windowErr == nil && e.BreakglassFile != "" {
			if _, err := os.Lstat(e.BreakglassFile); !errors.Is(err, os.ErrNotExist) {
				policyErr = e.invalidateWindowLocked(policyErr)
			}
		}
	}()
	empty := ApproverPolicy{Principals: map[string]PrincipalPolicy{}}
	if e.PolicyFile != "" {
		b, err := os.ReadFile(e.PolicyFile)
		if err == nil {
			e.policySeen = true
			var p ApproverPolicy
			if err := decodeStrict(b, &p); err != nil {
				return empty, false, fmt.Errorf("approver policy: %w", err)
			}
			if p.Principals == nil || len(p.Principals) > 256 || len(b) > 1<<20 {
				return empty, false, errors.New("approver policy needs principals")
			}
			if _, err := windowCap(p); err != nil {
				return empty, false, err
			}
			for urn, v := range p.Principals {
				if !validApproverURN(urn) || len(v.Scopes) != 1 || v.Scopes[0] != ApproveScope || v.RateLimit < 1 || v.RateLimit > 10000 {
					return empty, false, errors.New("invalid approver principal, scope or rate_limit (1-10000/hour required)")
				}
				names := map[string]bool{}
				for _, r := range v.Allowlist {
					if r.Name == "" || len(r.Name) > 128 || names[r.Name] || len(r.Paths) == 0 || len(r.Paths) > 128 || len(r.Actions) == 0 || len(r.Actions) > 4 {
						return empty, false, errors.New("invalid allowlist rule")
					}
					names[r.Name] = true
					for _, g := range r.Paths {
						if strings.HasPrefix(g, "/") || strings.Contains(g, "..") || strings.Contains(g, "\\") {
							return empty, false, errors.New("invalid path glob")
						}
						if _, err := path.Match(g, ""); err != nil {
							return empty, false, err
						}
					}
					for _, a := range r.Actions {
						if !contains([]string{"create", "modify", "delete", "unchanged"}, a) {
							return empty, false, errors.New("invalid allowlist action")
						}
					}
				}
			}
			return p, false, nil
		}
		if !errors.Is(err, os.ErrNotExist) || e.policySeen {
			return empty, false, errors.New("approver policy unavailable")
		}
	}
	// Preserve the old Tachyon service family only in the absent-file default.
	// The API retains its legacy catalog.write check for this fallback alone.
	return empty, true, nil
}
func (e *Engine) principalLocked(p ApproverPolicy, legacy bool, urn string) (PrincipalPolicy, bool) {
	if v, ok := p.Principals[urn]; ok {
		return v, true
	}
	if legacy {
		for _, app := range e.DelegatedApps {
			if urn == "msg://service/"+app || strings.HasPrefix(urn, "msg://service/"+app+"/") {
				return PrincipalPolicy{Scopes: []string{ApproveScope}, Privileged: e.DelegatedPrivileged, Allowlist: []ApprovalRule{{Name: "legacy-" + app, Paths: []string{"*"}, Actions: []string{"create", "modify", "delete", "unchanged"}}}}, true
			}
		}
	}
	return PrincipalPolicy{}, false
}

// LegacyDelegatedApprover identifies only the absent-policy compatibility path.
func (e *Engine) LegacyDelegatedApprover(urn string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, l, err := e.policyLocked()
	_, ok := e.principalLocked(p, l, urn)
	return err == nil && l && ok
}
func windowCap(p ApproverPolicy) (time.Duration, error) {
	if p.MaxWindow == "" {
		return maxBreakglass, nil
	}
	d, err := time.ParseDuration(p.MaxWindow)
	if err != nil || d <= 0 || d > maxBreakglass {
		return 0, errors.New("max_window must be positive and at most 4h")
	}
	return d, nil
}
func policyDigest(p ApproverPolicy) string {
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (e *Engine) authorizeDelegateLocked(req ApprovalRequest, plan *Plan) (string, error) {
	p, legacy, err := e.policyLocked()
	if err != nil {
		return "", err
	}
	v, ok := e.principalLocked(p, legacy, req.Principal)
	if !ok {
		return "", errors.New("principal is not a configured approver")
	}
	// These checks precede breakglass, including privileged permission.
	if len(plan.Privileged) > 0 && !v.Privileged {
		return "", errors.New("privileged changes: only the operator approves those unless explicitly permitted")
	}
	for _, f := range plan.Files {
		// State files cannot normally be bundled at all; keep this unconditional
		// even if a future catalog allowlist adds them. global.yaml controls state
		// roots and operator trust, so a delegate never approves it here.
		base := filepath.Base(f.Path)
		if strings.HasPrefix(req.Principal, "msg://agent/") {
			id := path.Base(req.Principal)
			id, _, _ = strings.Cut(id, "#")
			if f.Path == "agents/"+id+".yaml" || f.Path == "agents/"+id+".yml" || f.Path == "launches/"+id+".yaml" || f.Path == "launches/"+id+".yml" {
				return "", errors.New("delegate cannot approve its own definition")
			}
		}
		if f.Path == "global.yaml" || strings.HasPrefix(base, "catalog-approver") || base == "catalog-breakglass.json" {
			return "", errors.New("delegate cannot approve approver configuration or trust root")
		}
		for _, g := range req.ProtectedPaths {
			match, _ := path.Match(g, f.Path)
			if match || g == f.Path {
				return "", errors.New("delegate cannot approve its own definition")
			}
		}
	}
	now := e.now().UTC()
	if now.IsZero() || (!e.policyClock.IsZero() && now.Before(e.policyClock)) {
		return "", e.invalidateWindowLocked(errors.New("approval clock moved backwards"))
	}
	e.policyClock = now
	if e.approvalTimes == nil {
		e.approvalTimes = map[string][]time.Time{}
	}
	var recent []time.Time
	for _, at := range e.approvalTimes[req.Principal] {
		if now.Before(at.Add(time.Hour)) {
			recent = append(recent, at)
		}
	}
	e.approvalTimes[req.Principal] = recent
	if v.RateLimit > 0 && len(recent) >= v.RateLimit {
		return "", errors.New("approver rate limit exceeded")
	}
	if err := e.windowLocked(p, now); err != nil {
		return "", err
	}
	if e.window != nil && e.window.Principal == req.Principal {
		return "breakglass", nil
	}
	var matches []string
	for _, f := range plan.Files {
		found := ""
		for _, r := range v.Allowlist {
			if !contains(r.Actions, f.Action) {
				continue
			}
			for _, g := range r.Paths {
				match, _ := path.Match(g, f.Path)
				if legacy {
					match = true
				}
				if match {
					found = r.Name
					break
				}
			}
			if found != "" {
				break
			}
		}
		if found == "" {
			return "", fmt.Errorf("no approver allowlist rule for %s (%s)", f.Path, f.Action)
		}
		matches = append(matches, found)
	}
	return strings.Join(matches, ","), nil
}

func (e *Engine) approvalAudit(event, principal, rule, digest, plan string) error {
	if err := os.MkdirAll(e.BackupRoot, 0700); err != nil {
		return err
	}
	record := AuditRecord{At: e.now().UTC().Format(time.RFC3339Nano), Event: event, Principal: principal, Rule: rule, Digest: digest, PlanID: plan}
	if e.window != nil && (event == "breakglass_open" || rule == "breakglass") {
		record.Reason = e.window.Reason
		expires := e.window.ExpiresAt
		record.WindowExpiresAt = &expires
	}
	b, err := json.Marshal(record)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(e.BackupRoot, auditFile), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	cerr := f.Close()
	if err != nil {
		return err
	}
	return cerr
}

// BreakglassTrust is supplied by the existing daemon issuer. It is never
// reachable as a generic signing API, and never creates a second signing key.
type BreakglassTrust interface {
	SealCatalogBreakglass([]byte) (string, error)
	VerifyCatalogBreakglass([]byte, string) error
}

func (e *Engine) operatorDigest() (string, error) {
	if e.Approver == nil {
		return "", ErrNoPassphrase
	}
	f, err := e.Approver.load()
	if err != nil {
		return "", err
	}
	b, _ := json.Marshal(f)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

type sealedWindow struct {
	Window    *BreakglassWindow `json:"window"`
	Signature string            `json:"signature"`
}

func (e *Engine) saveWindowLocked() error {
	if e.BreakglassFile == "" {
		return errors.New("breakglass persistence path is not configured")
	}
	b, err := json.Marshal(e.window)
	if err != nil {
		return err
	}
	if e.WindowTrust == nil {
		return errors.New("breakglass issuer trust unavailable")
	}
	mac, err := e.WindowTrust.SealCatalogBreakglass(b)
	if err != nil {
		return err
	}
	data, err := json.Marshal(sealedWindow{e.window, mac})
	if err != nil {
		return err
	}
	if err := writeWindowState(e.BreakglassFile, data); err != nil {
		return err
	}
	e.windowSignature = mac
	return nil
}

// writeWindowState makes both the replacement and its directory entry durable.
func writeWindowState(destination string, data []byte) error {
	dir := filepath.Dir(destination)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".breakglass-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name) //nolint:errcheck
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(name, destination); err != nil {
		return err
	}
	return syncWindowDirectory(destination)
}
func syncWindowDirectory(destination string) error {
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close() //nolint:errcheck
	return dir.Sync()
}

// This denial-only marker needs no signing authority: its presence can only
// remove authority. It is durable even when the existing signer is unavailable.
// Only a successful passphrase-gated end removes it, after persisting closure.
func (e *Engine) blockWindowLocked() error {
	if e.BreakglassFile == "" {
		return nil
	}
	return writeWindowState(e.BreakglassFile+".blocked", []byte("operator end required\n"))
}

func (e *Engine) invalidateWindowLocked(err error) error {
	if persistErr := e.blockWindowLocked(); persistErr != nil {
		err = errors.Join(err, fmt.Errorf("persist breakglass denial: %w", persistErr))
	}
	e.window = nil
	if e.windowTimer != nil {
		e.windowTimer.Stop()
		e.windowTimer = nil
	}
	e.windowErr = err
	_ = e.approvalAudit("breakglass_invalid", "", "", "", "")
	slog.Error("catalog breakglass closed after invalid state", "error", err)
	return err
}
func (e *Engine) windowLocked(p ApproverPolicy, now time.Time) error {
	if e.windowErr != nil {
		return e.windowErr
	}
	if err := e.checkWindowLocked(p, now); err != nil {
		return e.invalidateWindowLocked(err)
	}
	return nil
}
func (e *Engine) checkWindowLocked(p ApproverPolicy, now time.Time) error {
	if e.BreakglassFile != "" {
		_, err := os.Lstat(e.BreakglassFile + ".blocked")
		if err == nil {
			return errors.New("breakglass is blocked; operator end required")
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if !e.windowLoaded {
		e.windowLoaded = true
		if e.BreakglassFile != "" {
			b, err := os.ReadFile(e.BreakglassFile)
			if err == nil {
				var seal sealedWindow
				if err = decodeStrict(b, &seal); err != nil {
					return err
				}
				body, _ := json.Marshal(seal.Window)
				if e.WindowTrust == nil || e.WindowTrust.VerifyCatalogBreakglass(body, seal.Signature) != nil {
					return errors.New("breakglass verification failed")
				}
				e.window = seal.Window
				e.windowSignature = seal.Signature
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	w := e.window
	if w == nil {
		return nil
	}
	body, _ := json.Marshal(w)
	if e.WindowTrust == nil || e.WindowTrust.VerifyCatalogBreakglass(body, e.windowSignature) != nil {
		return errors.New("breakglass issuer trust changed")
	}
	windowLimit, err := windowCap(p)
	operatorDigest, trustErr := e.operatorDigest()
	if trustErr != nil || operatorDigest != w.OperatorDigest || err != nil || now.IsZero() || w.OpenedAt.IsZero() || w.LastSeen.Before(w.OpenedAt) || now.Before(w.LastSeen) || !w.ExpiresAt.After(w.OpenedAt) || w.ExpiresAt.Sub(w.OpenedAt) > windowLimit || w.PolicyDigest != policyDigest(p) {
		e.window = nil
		return errors.New("breakglass invalid policy or clock; closed")
	}
	if !now.Before(w.ExpiresAt) {
		e.window = nil
		if e.windowTimer != nil {
			e.windowTimer.Stop()
			e.windowTimer = nil
		}
		if err := e.saveWindowLocked(); err != nil {
			return err
		}
		if err := e.approvalAudit("breakglass_expire", w.Principal, "", "", ""); err != nil {
			return err
		}
		slog.Warn("catalog breakglass expired", "principal", w.Principal)
		return nil
	}
	e.scheduleExpiryLocked(now)
	w.LastSeen = now
	if err := e.saveWindowLocked(); err != nil {
		e.window = nil
		return err
	}
	return nil
}

// Breakglass requires the same operator passphrase as plan approval. The HTTP
// handler additionally requires the existing local operator admission gate.
func (e *Engine) Breakglass(principal, reason string, duration time.Duration, passphrase string, end bool) (*BreakglassWindow, error) {
	if e.Approver == nil {
		return nil, ErrNoPassphrase
	}
	if err := e.Approver.Verify(passphrase); err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now().UTC()
	if end {
		old := e.window
		// Persist denial BEFORE clearing memory or attempting a signed closure.
		if err := e.blockWindowLocked(); err != nil {
			return nil, e.invalidateWindowLocked(err)
		}
		if e.windowTimer != nil {
			e.windowTimer.Stop()
			e.windowTimer = nil
		}
		e.window = nil
		e.windowLoaded = true
		if err := e.saveWindowLocked(); err != nil {
			return nil, e.invalidateWindowLocked(err)
		}
		if old != nil {
			principal = old.Principal
		}
		if err := e.approvalAudit("breakglass_end", principal, "", "", ""); err != nil {
			return nil, e.invalidateWindowLocked(err)
		}
		if err := os.Remove(e.BreakglassFile + ".blocked"); err != nil {
			return nil, e.invalidateWindowLocked(err)
		}
		if err := syncWindowDirectory(e.BreakglassFile); err != nil {
			return nil, e.invalidateWindowLocked(err)
		}
		e.windowErr = nil
		e.policyClock = now
		slog.Warn("catalog breakglass closed", "principal", principal)
		return nil, nil
	}

	p, legacy, err := e.policyLocked()
	if err != nil {
		return nil, err
	}
	if now.IsZero() || (!e.policyClock.IsZero() && now.Before(e.policyClock)) {
		return nil, e.invalidateWindowLocked(errors.New("breakglass clock moved backwards"))
	}
	if err := e.windowLocked(p, now); err != nil {
		return nil, err
	}
	if e.window != nil {
		return nil, errors.New("one breakglass window is already active; end it first")
	}
	if _, ok := e.principalLocked(p, legacy, principal); !ok {
		return nil, errors.New("unknown approver principal")
	}
	windowLimit, err := windowCap(p)
	if err != nil {
		return nil, err
	}
	if duration <= 0 || duration > windowLimit || now.IsZero() || strings.TrimSpace(reason) == "" || len(reason) > 512 {
		return nil, errors.New("breakglass needs a reason and positive duration within max_window")
	}
	operatorDigest, err := e.operatorDigest()
	if err != nil {
		return nil, err
	}
	e.window = &BreakglassWindow{OperatorDigest: operatorDigest, Principal: principal, Reason: reason, OpenedAt: now, ExpiresAt: now.Add(duration), LastSeen: now, PolicyDigest: policyDigest(p)}
	if err := e.approvalAudit("breakglass_open", principal, "", "", ""); err != nil {
		e.window = nil
		return nil, err
	}
	if err := e.saveWindowLocked(); err != nil {
		e.window = nil
		return nil, err
	}
	slog.Warn("catalog breakglass opened", "principal", principal, "expires_at", e.window.ExpiresAt)
	e.scheduleExpiryLocked(now)
	windowCopy := *e.window
	return &windowCopy, nil
}
func (e *Engine) Approvers() (ApproverView, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, l, err := e.policyLocked()
	if err != nil {
		return ApproverView{}, err
	}
	if err = e.windowLocked(p, e.now().UTC()); err != nil {
		return ApproverView{}, err
	}
	v := ApproverView{Policy: p, LegacyTachyon: l && contains(e.DelegatedApps, "tachyon")}
	if l {
		v.LegacyDefaults = map[string]PrincipalPolicy{}
		for _, app := range e.DelegatedApps {
			urn := "msg://service/" + app
			rule, _ := e.principalLocked(p, l, urn)
			v.LegacyDefaults[urn+"[/<env>]"] = rule
		}
	}
	if e.window != nil {
		w := *e.window
		v.Breakglass = &w
	}
	return v, nil
}

// Timer closes and audits the window without requiring another request. Every
// approval also checks the absolute deadline synchronously.
func (e *Engine) scheduleExpiryLocked(now time.Time) {
	if e.window == nil || e.windowTimer != nil {
		return
	}
	scheduledWindow := e.window
	e.windowTimer = time.AfterFunc(e.window.ExpiresAt.Sub(now), func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		// A stopped timer can already be waiting for this lock. It must
		// never close a replacement window opened in the meantime.
		if e.window != scheduledWindow {
			return
		}
		e.windowTimer = nil
		if e.now().UTC().Before(scheduledWindow.ExpiresAt) {
			_ = e.invalidateWindowLocked(errors.New("breakglass expiry clock did not advance"))
			return
		}
		p, _, err := e.policyLocked()
		if err == nil {
			err = e.windowLocked(p, e.now().UTC())
		}
		if err != nil {
			_ = e.invalidateWindowLocked(err)
		}
	})
}
