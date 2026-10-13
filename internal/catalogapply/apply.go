package catalogapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Result is the outcome of Apply.
type Result struct {
	PlanID     string `json:"plan_id"`
	Applied    bool   `json:"applied"`
	BackupID   string `json:"backup_id"`
	Activation string `json:"activation"`
	// RolledBack is true when the install failed and the touched files were restored.
	RolledBack bool     `json:"rolled_back,omitempty"`
	Changed    []string `json:"changed"`
	// AuditNote is a one-line summary for the Torque audit comment.
	AuditNote string `json:"audit_note"`
}

// Activation values.
const (
	ActivationImmediate       = "immediate"
	ActivationRestartRequired = "restart_required"
)

// Apply installs an approved plan. It re-checks the live catalog against the
// plan's base, backs up the whole catalog, installs file by file and
// restores the touched files if anything fails, including a catalog that no
// longer validates after the install.
func (e *Engine) Apply(ctx context.Context, planID string) (*Result, error) {
	e.applyMu.Lock()
	defer e.applyMu.Unlock()

	e.mu.Lock()
	p, ok := e.plans[planID]
	if !ok {
		e.mu.Unlock()
		return nil, fmt.Errorf("plan %q not found or expired", planID)
	}
	cp := clonePlan(p)
	e.mu.Unlock()

	if e.now().After(cp.ExpiresAt) {
		return nil, fmt.Errorf("plan %q expired; plan again", planID)
	}
	if cp.State == StateApplied {
		return nil, fmt.Errorf("plan %q is already applied", planID)
	}
	if cp.State != StateApproved || cp.ApprovedAt == nil {
		return nil, fmt.Errorf("plan %q is not approved (state %s); a human approves a plan with its digest", planID, cp.State)
	}
	if digestOf(cp) != cp.Digest {
		return nil, fmt.Errorf("plan %q digest no longer matches its content", planID)
	}
	// The live catalog must still be what the approver reviewed.
	for _, f := range cp.Files {
		live := "-"
		if b, err := os.ReadFile(filepath.Join(e.Root, filepath.FromSlash(f.Path))); err == nil {
			live = sha(string(b))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if live != f.baseSHA256 {
			return nil, fmt.Errorf("stale plan: %s changed after the plan was made; plan again", f.Path)
		}
		if err := checkNoSymlinks(e.Root, f.Path); err != nil {
			return nil, err
		}
	}
	if e.RunningSessions != nil && len(cp.Touches) > 0 && !cp.AllowRunning {
		ids, err := e.RunningSessions(ctx, cp.Touches)
		if err != nil {
			return nil, fmt.Errorf("check running sessions: %w", err)
		}
		if len(ids) > 0 {
			return nil, fmt.Errorf("sessions %v now use the touched definitions; stop them or re-approve allowing it", ids)
		}
	}

	backupID := "catalog-pre-" + cp.Name + "-" + e.now().UTC().Format("20060102T150405Z")
	backupDir := filepath.Join(e.BackupRoot, backupID)
	if err := os.MkdirAll(e.BackupRoot, 0o700); err != nil {
		return nil, fmt.Errorf("backup root: %w", err)
	}
	if err := copyTree(e.Root, backupDir); err != nil {
		_ = os.RemoveAll(backupDir)
		return nil, fmt.Errorf("backup: %w", err)
	}

	var changed []string
	restore := func() {
		for _, c := range changed {
			src := filepath.Join(backupDir, filepath.FromSlash(c))
			dst := filepath.Join(e.Root, filepath.FromSlash(c))
			if b, err := os.ReadFile(src); err == nil { //nolint:gosec // G304: src is a validated catalog-relative path under the backup the daemon just made
				_ = writeAtomic(dst, b)
			} else {
				_ = os.Remove(dst)
			}
		}
	}
	for _, f := range cp.Files {
		if f.Action == "unchanged" {
			continue
		}
		dst := filepath.Join(e.Root, filepath.FromSlash(f.Path))
		changed = append(changed, f.Path)
		var err error
		if f.Action == "delete" {
			err = os.Remove(dst)
		} else {
			err = writeAtomic(dst, []byte(f.content))
		}
		if err != nil {
			restore()
			return &Result{PlanID: planID, BackupID: backupID, RolledBack: true, Changed: changed}, fmt.Errorf("install %s: %w (restored)", f.Path, err)
		}
	}
	if verr := e.validate(e.Root); verr != nil {
		restore()
		return &Result{PlanID: planID, BackupID: backupID, RolledBack: true, Changed: changed}, fmt.Errorf("catalog invalid after install: %w (restored from %s)", verr, backupID)
	}

	res := &Result{PlanID: planID, Applied: true, BackupID: backupID, Changed: changed, Activation: activationOf(cp)}
	res.AuditNote = fmt.Sprintf("catalog apply %q (plan %s, digest %s): %s; backup %s; activation %s", cp.Name, cp.ID, cp.Digest[:12], strings.Join(changed, ", "), backupID, res.Activation)

	now := e.now().UTC()
	e.mu.Lock()
	if q, ok := e.plans[planID]; ok {
		q.State, q.AppliedAt, q.BackupID, q.Activation = StateApplied, &now, backupID, res.Activation
	}
	e.mu.Unlock()
	e.audit(res, cp)
	return res, nil
}

// activationOf says whether the daemon sees the change without a restart:
// only launches are re-read from disk per launch today.
func activationOf(p *Plan) string {
	for _, f := range p.Files {
		if f.Action == "unchanged" {
			continue
		}
		if !strings.HasPrefix(f.Path, "launches/") {
			return ActivationRestartRequired
		}
	}
	return ActivationImmediate
}

func (e *Engine) audit(res *Result, p *Plan) {
	rec := map[string]any{
		"at": e.now().UTC().Format("2006-01-02T15:04:05Z"), "plan_id": p.ID, "name": p.Name, "task_id": p.TaskID,
		"digest": p.Digest, "files": res.Changed, "backup_id": res.BackupID, "activation": res.Activation,
		"privileged": p.Privileged, "rollback_of": p.RollbackOf,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(e.BackupRoot, "apply-audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// PlanRollback plans restoring the allowlisted catalog files to the state in
// backupID. It goes through the same review, approval and apply as any change.
func (e *Engine) PlanRollback(ctx context.Context, backupID string, opt Options) (*Plan, error) {
	if backupID == "" || filepath.Base(backupID) != backupID || !strings.HasPrefix(backupID, "catalog-pre-") {
		return nil, fmt.Errorf("backup id %q is not a catalog backup", backupID)
	}
	dir := filepath.Join(e.BackupRoot, backupID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("backup %q not found", backupID)
	}
	live, err := listAllowed(e.Root)
	if err != nil {
		return nil, err
	}
	back, err := listAllowed(dir)
	if err != nil {
		return nil, err
	}
	var files []File
	for p, c := range back {
		if lc, ok := live[p]; !ok || lc != c {
			files = append(files, File{Path: p, Content: c})
		}
	}
	for p := range live {
		if _, ok := back[p]; !ok {
			files = append(files, File{Path: p, Delete: true})
		}
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("the catalog already matches backup %q", backupID)
	}
	name := "rollback-" + strings.TrimPrefix(backupID, "catalog-pre-")
	if len(name) > 64 {
		name = name[:64]
	}
	p, err := e.plan(ctx, Bundle{Name: name, Files: files}, opt, true)
	if err != nil {
		return nil, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if q, ok := e.plans[p.ID]; ok {
		q.RollbackOf = backupID
		q.Digest = digestOf(q)
		p = clonePlan(q)
	}
	return p, nil
}

// listAllowed reads every allowlisted catalog file under root.
func listAllowed(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if verr := ValidatePath(rel); verr != nil {
			return nil //nolint:nilerr // a file outside the allowlist is skipped, not an error
		}
		b, err := os.ReadFile(path) //nolint:gosec // G304/G122: path comes from walking the catalog or a backup root the daemon owns
		if err != nil {
			return err
		}
		if len(b) <= maxFileBytes {
			out[rel] = string(b)
		}
		return nil
	})
	return out, err
}

func writeAtomic(dst string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { //nolint:gosec // G301: catalog directories are 0755, as the installer and the sysop create them
		return err
	}
	mode := os.FileMode(0o644)
	if fi, err := os.Stat(dst); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".apply-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, dst)
}

// copyTree copies a catalog tree, preserving file modes. Symlinks are
// recreated as absolute links so a copy elsewhere resolves the same targets.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			l, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if !filepath.IsAbs(l) {
				l = filepath.Join(filepath.Dir(path), l)
			}
			return os.Symlink(l, target) //nolint:gosec // G122: copying a catalog the daemon owns; links are recreated, never followed
		case d.Type().IsRegular():
			in, err := os.Open(path) //nolint:gosec // G304/G122: path comes from walking a catalog tree the daemon owns
			if err != nil {
				return err
			}
			defer in.Close()
			fi, err := in.Stat()
			if err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, fi.Mode().Perm()|0o600) //nolint:gosec // G304: target is under a fresh scratch or backup directory
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}
