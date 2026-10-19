// Package catalogapply plans, approves, installs and rolls back catalog
// changes as one reviewed unit (CW-20261012-0124).
//
// A change is a Bundle of files. Plan checks every path against an allowlist
// and every body for obvious secrets, applies the bundle to a scratch copy of
// the catalog, validates the copy and returns a diff and a digest. Nothing in
// the live catalog changes. A human approves a plan by digest with a
// passphrase (Approver); Apply then re-checks, backs up the whole catalog and
// installs file by file, restoring on any failure. The same path restores a
// backup (PlanRollback).
//
// The daemon, not the agent's `tether mcp` process, owns every write here.
package catalogapply

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/hollis-labs/tether/internal/catalogcheck"
)

// File is one catalog file in a bundle. Delete removes it.
type File struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
	Delete  bool   `json:"delete,omitempty"`
}

// Bundle is a requested change.
type Bundle struct {
	Name   string `json:"name"`
	TaskID string `json:"task_id,omitempty"`
	Files  []File `json:"files"`
}

// Plan states.
const (
	StatePlanned  = "planned"
	StateInvalid  = "invalid"
	StateApproved = "approved"
	StateApplied  = "applied"
)

// PlannedFile is one change a plan would make.
type PlannedFile struct {
	Path       string `json:"path"`
	Action     string `json:"action"` // create, modify, delete, unchanged
	Privileged bool   `json:"privileged,omitempty"`
	NewSHA256  string `json:"new_sha256,omitempty"`
	baseSHA256 string
	content    string
}

// Plan is a reviewed-but-not-installed change.
type Plan struct {
	ID               string        `json:"plan_id"`
	Name             string        `json:"name"`
	TaskID           string        `json:"task_id,omitempty"`
	State            string        `json:"state"`
	Digest           string        `json:"digest"`
	CreatedAt        time.Time     `json:"created_at"`
	ExpiresAt        time.Time     `json:"expires_at"`
	TTLSeconds       int           `json:"ttl_seconds"`
	Files            []PlannedFile `json:"files"`
	Diff             string        `json:"diff"`
	ValidationOK     bool          `json:"validation_ok"`
	ValidationError  string        `json:"validation_error,omitempty"`
	BaselineError    string        `json:"baseline_error,omitempty"`
	Privileged       []string      `json:"privileged,omitempty"`
	RunningSessions  []string      `json:"running_sessions,omitempty"`
	Touches          []string      `json:"touches,omitempty"`
	RollbackOf       string        `json:"rollback_of,omitempty"`
	PlannedBy        Caller        `json:"planned_by"`
	ApprovedAt       *time.Time    `json:"approved_at,omitempty"`
	AllowRunning     bool          `json:"allow_running,omitempty"`
	AppliedAt        *time.Time    `json:"applied_at,omitempty"`
	BackupID         string        `json:"backup_id,omitempty"`
	Activation       string        `json:"activation,omitempty"`
	ApprovalRequired string        `json:"approval_required,omitempty"`
}

// Caller names who planned a change: the verified principal and, for a
// launched agent, its session and agent. It is recorded, not trusted for access.
type Caller struct {
	Principal string `json:"principal,omitempty"` // principal kind:id, or "local" for an unauthenticated local caller
	SessionID string `json:"session_id,omitempty"`
	AgentID   string `json:"agent_id,omitempty"`
}

// Options carries the caller's context into planning.
type Options struct {
	expectedBase map[string]string
	// Caller identifies who asked, for the audit record. Zero means unknown.
	Caller Caller
	// ProtectedPaths are catalog paths the caller may not change (its own
	// definition). A plan touching one is refused.
	ProtectedPaths []string
}

// Engine plans and applies catalog changes under Root.
type Engine struct {
	Root       string
	BackupRoot string
	Approver   *Approver
	// Validate checks a catalog tree; nil uses the daemon's own loaders.
	Validate func(root string) error
	// RunningSessions returns ids of live sessions that use any of touches
	// ("agent:<id>", "launch:<id>"). Nil means none are known.
	RunningSessions func(ctx context.Context, touches []string) ([]string, error)
	// Reload swaps the installed catalog into the running daemon after an
	// apply; restartRequired names edits it could not apply live. Nil means
	// no live reload (only launches/ files are then immediate).
	Reload func(ctx context.Context) (restartRequired []string, err error)
	Now    func() time.Time
	TTL    time.Duration

	mu      sync.Mutex
	plans   map[string]*Plan
	applyMu sync.Mutex
}

const maxPlans = 64

// rearmGrace keeps an expired plan around so planning the same bundle again
// can re-arm it. Approve and Apply still refuse it once it has expired.
const rearmGrace = time.Hour

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) ttl() time.Duration {
	if e.TTL > 0 {
		return e.TTL
	}
	return 15 * time.Minute
}

func (e *Engine) validate(root string) error {
	if e.Validate != nil {
		return e.Validate(root)
	}
	return defaultValidate(root)
}

func defaultValidate(root string) error {
	rep := catalogcheck.Run(root)
	if rep.OK {
		return nil
	}
	var msgs []string
	for _, r := range rep.Results {
		if r.Status == catalogcheck.StatusFail {
			msgs = append(msgs, r.Name+": "+r.Message)
		}
	}
	return errors.New(strings.Join(msgs, "; "))
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "cp_" + hex.EncodeToString(b)
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// PlanBundle reviews a bundle without touching the live catalog.
func (e *Engine) PlanBundle(ctx context.Context, b Bundle, opt Options) (*Plan, error) {
	return e.plan(ctx, b, opt, false)
}

// plan is PlanBundle; skipSecretScan is for a rollback, which restores
// content the catalog already held (existing upstream definitions may carry
// literals this tool would refuse to author).
func (e *Engine) plan(ctx context.Context, b Bundle, opt Options, skipSecretScan bool) (*Plan, error) {
	if !nameRe.MatchString(b.Name) {
		return nil, fmt.Errorf("bundle name %q must be 1-64 characters of letters, digits, '.', '_' or '-'", b.Name)
	}
	if len(b.Files) == 0 {
		return nil, fmt.Errorf("bundle %q has no files", b.Name)
	}
	files, err := e.checkFiles(b.Files, opt, skipSecretScan)
	if err != nil {
		return nil, err
	}

	scratch, err := os.MkdirTemp("", "tether-catalog-plan-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	scratchRoot := filepath.Join(scratch, "catalog")
	if err := copyTree(e.Root, scratchRoot); err != nil {
		return nil, fmt.Errorf("copy catalog for review: %w", err)
	}

	p := &Plan{
		ID: newID(), Name: b.Name, TaskID: b.TaskID, State: StatePlanned,
		CreatedAt: e.now().UTC(), PlannedBy: opt.Caller,
	}
	p.ExpiresAt = p.CreatedAt.Add(e.ttl())
	p.TTLSeconds = int(e.ttl() / time.Second)

	var diff strings.Builder
	touches := map[string]bool{}
	privSet := map[string]bool{}
	for _, f := range files {
		live := filepath.Join(e.Root, filepath.FromSlash(f.Path))
		oldB, rerr := os.ReadFile(live) //nolint:gosec // G304: live is Root joined with an allowlist-validated relative path
		exists := rerr == nil
		if rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
			return nil, rerr
		}
		old := string(oldB)
		pf := PlannedFile{Path: f.Path, baseSHA256: "-", content: f.Content}
		if exists {
			pf.baseSHA256 = sha(old)
		}
		if expected, ok := opt.expectedBase[f.Path]; ok && expected != pf.baseSHA256 {
			return nil, errors.New("catalog changed while preparing activation plan")
		}
		switch {
		case f.Delete && !exists:
			return nil, fmt.Errorf("%s: cannot delete, it does not exist", f.Path)
		case f.Delete:
			pf.Action = "delete"
		case !exists:
			pf.Action = "create"
			pf.NewSHA256 = sha(f.Content)
		case old == f.Content:
			pf.Action = "unchanged"
			pf.NewSHA256 = sha(f.Content)
		default:
			pf.Action = "modify"
			pf.NewSHA256 = sha(f.Content)
		}
		dst := filepath.Join(scratchRoot, filepath.FromSlash(f.Path))
		switch pf.Action {
		case "delete":
			_ = os.Remove(dst)
		case "create", "modify", "unchanged":
			if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: scratch copy of the catalog, removed after review
				return nil, err
			}
			if err := os.WriteFile(dst, []byte(f.Content), 0o644); err != nil { //nolint:gosec // G306: scratch copy; catalog files are 0644
				return nil, err
			}
		}
		if pf.Action != "unchanged" {
			if reasons := append(privilegedReasons(f.Path, old, f.Content, f.Delete), e.teamLaunchReasons(f.Path)...); len(reasons) > 0 {
				pf.Privileged = true
				for _, r := range reasons {
					privSet[f.Path+": "+r] = true
				}
			}
			diff.WriteString(unifiedDiff(f.Path, old, f.Content))
			if t := touchOf(f.Path); t != "" {
				touches[t] = true
			}
		}
		p.Files = append(p.Files, pf)
	}
	p.Diff = diff.String()
	for k := range privSet {
		p.Privileged = append(p.Privileged, k)
	}
	sort.Strings(p.Privileged)
	for k := range touches {
		p.Touches = append(p.Touches, k)
	}
	sort.Strings(p.Touches)

	if berr := e.validate(e.Root); berr != nil {
		p.BaselineError = berr.Error()
	}
	if verr := e.validate(scratchRoot); verr != nil {
		p.ValidationError = verr.Error()
	} else {
		p.ValidationOK = true
	}
	if !p.ValidationOK {
		p.State = StateInvalid
	}
	changed := false
	for _, f := range p.Files {
		if f.Action != "unchanged" {
			changed = true
		}
	}
	if !changed {
		p.State = StateInvalid
		p.ValidationError = "the bundle changes nothing"
		p.ValidationOK = false
	}
	if e.RunningSessions != nil && len(p.Touches) > 0 {
		ids, rerr := e.RunningSessions(ctx, p.Touches)
		if rerr != nil {
			return nil, fmt.Errorf("check running sessions: %w", rerr)
		}
		p.RunningSessions = ids
	}
	p.Digest = digestOf(p)
	switch {
	case len(p.Privileged) > 0:
		p.ApprovalRequired = "the approver must confirm the privileged changes listed in this plan"
	case len(p.RunningSessions) > 0:
		p.ApprovalRequired = "sessions using the touched definitions are running; the approver must allow that explicitly"
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	e.gcLocked()
	// Planning the same unapproved bundle again re-arms the existing plan (same
	// id, fresh expiry) so an operator who was shown the id can still approve it.
	// The digest covers the live base hashes, so a catalog that moved on does not match.
	for id, old := range e.plans {
		if old.State == StatePlanned && old.ApprovedAt == nil && old.Digest == p.Digest && old.Name == p.Name {
			p.ID = id
			break
		}
	}
	if _, rearm := e.plans[p.ID]; !rearm && len(e.plans) >= maxPlans {
		return nil, fmt.Errorf("too many open plans (%d); apply or let them expire", maxPlans)
	}
	if e.plans == nil {
		e.plans = map[string]*Plan{}
	}
	e.plans[p.ID] = p
	return clonePlan(p), nil
}

func (e *Engine) gcLocked() {
	now := e.now()
	for id, p := range e.plans {
		if now.After(p.ExpiresAt.Add(rearmGrace)) && p.State != StateApplied {
			delete(e.plans, id)
		}
	}
}

func clonePlan(p *Plan) *Plan {
	c := *p
	c.Files = append([]PlannedFile(nil), p.Files...)
	return &c
}

func digestOf(p *Plan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "name=%s\nrollback_of=%s\n", p.Name, p.RollbackOf)
	for _, f := range p.Files {
		fmt.Fprintf(&sb, "%s\x00%s\x00%s\x00%s\n", f.Path, f.Action, f.NewSHA256, f.baseSHA256)
	}
	return sha(sb.String())
}

// touchOf names the definition a path belongs to, for the running-session check.
func touchOf(p string) string {
	switch {
	case strings.HasPrefix(p, "agents/"):
		return "agent:" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(p, "agents/"), ".yaml"), ".yml")
	case strings.HasPrefix(p, "launches/"):
		return "launch:" + strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(p, "launches/"), ".yaml"), ".yml")
	}
	return ""
}

// privilegedKeys are top-level keys of an agent or launch definition whose
// change widens or alters what the agent may do.
var privilegedKeys = []string{"permissions", "mcp_scopes", "mcp_grants", "grants", "sandbox", "network", "permission_mode", "mcp_servers", "extends"}

func privilegedReasons(p, old, next string, deleted bool) []string {
	var reasons []string
	if p == "teams-policy.yaml" {
		reasons = append(reasons, "changes team definition activation bindings; restart required")
	}
	if privilegedPath(p) {
		reasons = append(reasons, "changes daemon defaults, upstream servers or sandbox rules")
	}
	dir := strings.SplitN(p, "/", 2)[0]
	if dir != "agents" && dir != "launches" {
		return reasons
	}
	var o, n map[string]any
	_ = yaml.Unmarshal([]byte(old), &o)
	if !deleted {
		_ = yaml.Unmarshal([]byte(next), &n)
	}
	for _, k := range privilegedKeys {
		ov, nv := fmt.Sprint(o[k]), fmt.Sprint(n[k])
		if ov != nv {
			reasons = append(reasons, "changes "+k)
		}
	}
	return reasons
}

// checkFiles applies the input rules every bundle meets before anything is
// copied or written: allowlisted paths, no duplicates, no change to the
// caller's own definition, bounded non-empty content free of obvious secrets,
// no symlinked destinations. It returns the files sorted by path.
func (e *Engine) checkFiles(in []File, opt Options, skipSecretScan bool) ([]File, error) {
	protected := map[string]bool{}
	for _, p := range opt.ProtectedPaths {
		protected[p] = true
	}
	seen := map[string]bool{}
	files := append([]File(nil), in...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		if err := ValidatePath(f.Path); err != nil {
			return nil, err
		}
		if seen[f.Path] {
			return nil, fmt.Errorf("path %q appears twice in the bundle", f.Path)
		}
		seen[f.Path] = true
		if protected[f.Path] {
			return nil, fmt.Errorf("%s is the caller's own definition; an agent does not change its own definition or permissions", f.Path)
		}
		if f.Delete {
			if f.Content != "" {
				return nil, fmt.Errorf("%s: a delete carries no content", f.Path)
			}
		} else {
			if len(f.Content) > maxFileBytes {
				return nil, fmt.Errorf("%s: larger than %d bytes", f.Path, maxFileBytes)
			}
			if f.Content == "" {
				return nil, fmt.Errorf("%s: empty content (use delete to remove a file)", f.Path)
			}
			if why := ScanSecrets(f.Content); why != "" && !skipSecretScan {
				return nil, fmt.Errorf("%s: refused, the content %s; reference secrets instead of embedding them", f.Path, why)
			}
		}
		if err := checkNoSymlinks(e.Root, f.Path); err != nil {
			return nil, err
		}
	}
	return files, nil
}

// ValidateLive checks the live catalog and returns structured findings.
func (e *Engine) ValidateLive() catalogcheck.Report { return catalogcheck.Run(e.Root) }

// ValidateBundle checks what the catalog would be with the bundle applied,
// without storing a plan or touching the live catalog. An empty bundle is the
// live catalog.
func (e *Engine) ValidateBundle(files []File, opt Options) (catalogcheck.Report, error) {
	if len(files) == 0 {
		return e.ValidateLive(), nil
	}
	checked, err := e.checkFiles(files, opt, false)
	if err != nil {
		return catalogcheck.Report{}, err
	}
	scratch, err := os.MkdirTemp("", "tether-catalog-validate-*")
	if err != nil {
		return catalogcheck.Report{}, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	root := filepath.Join(scratch, "catalog")
	if err := copyTree(e.Root, root); err != nil {
		return catalogcheck.Report{}, fmt.Errorf("copy catalog for validation: %w", err)
	}
	for _, f := range checked {
		dst := filepath.Join(root, filepath.FromSlash(f.Path))
		if f.Delete {
			if err := os.Remove(dst); err != nil {
				return catalogcheck.Report{}, fmt.Errorf("%s: cannot delete, it does not exist", f.Path)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: scratch copy, removed after validation
			return catalogcheck.Report{}, err
		}
		if err := os.WriteFile(dst, []byte(f.Content), 0o644); err != nil { //nolint:gosec // G306: scratch copy; catalog files are 0644
			return catalogcheck.Report{}, err
		}
	}
	return catalogcheck.Run(root), nil
}

// Get returns a copy of a plan.
func (e *Engine) Get(id string) (*Plan, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.plans[id]
	if !ok {
		return nil, false
	}
	return clonePlan(p), true
}

// ApprovalRequest is a human's approval of one plan.
type ApprovalRequest struct {
	PlanID     string
	Digest     string // the digest the approver read; must equal the plan's
	Passphrase string
	// AckPrivileged confirms the privileged changes the plan lists.
	AckPrivileged bool
	// AllowRunning permits changing definitions that live sessions use.
	AllowRunning bool
}

// Approve records a human's approval. The approver must have read the plan's
// digest and know the approval passphrase.
func (e *Engine) Approve(req ApprovalRequest) (*Plan, error) {
	if e.Approver == nil {
		return nil, ErrNoPassphrase
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	p, ok := e.plans[req.PlanID]
	if !ok {
		return nil, fmt.Errorf("plan %q not found or expired", req.PlanID)
	}
	if e.now().After(p.ExpiresAt) {
		return nil, fmt.Errorf("plan %q expired; plan the same bundle again to re-arm it", req.PlanID)
	}
	if p.State == StateInvalid {
		return nil, fmt.Errorf("plan %q is not valid and cannot be approved: %s", p.ID, p.ValidationError)
	}
	if p.State == StateApplied {
		return nil, fmt.Errorf("plan %q is already applied", p.ID)
	}
	if req.Digest != p.Digest {
		return nil, fmt.Errorf("digest mismatch: the approval names a different plan content than %q holds", p.ID)
	}
	if len(p.Privileged) > 0 && !req.AckPrivileged {
		return nil, fmt.Errorf("plan %q makes privileged changes; confirm them explicitly", p.ID)
	}
	if len(p.RunningSessions) > 0 && !req.AllowRunning {
		return nil, fmt.Errorf("plan %q touches definitions used by running sessions %v; allow that explicitly or stop them", p.ID, p.RunningSessions)
	}
	if err := e.Approver.Verify(req.Passphrase); err != nil {
		return nil, err
	}
	now := e.now().UTC()
	p.State, p.ApprovedAt, p.AllowRunning = StateApproved, &now, req.AllowRunning
	return clonePlan(p), nil
}

// BackupInfo describes one catalog backup.
type BackupInfo struct {
	ID        string    `json:"backup_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Backups lists backups, newest first.
func (e *Engine) Backups() ([]BackupInfo, error) {
	ents, err := os.ReadDir(e.BackupRoot)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BackupInfo
	for _, d := range ents {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "catalog-pre-") {
			continue
		}
		if fi, err := d.Info(); err == nil {
			out = append(out, BackupInfo{ID: d.Name(), CreatedAt: fi.ModTime().UTC()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}
