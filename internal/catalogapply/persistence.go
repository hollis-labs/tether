package catalogapply

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// persistedPlan deliberately has no state, approval or derived security flags.
// Bundle contents are scanned; diffs and live base contents are never stored.
type persistedPlan struct {
	ID         string            `json:"id"`
	Bundle     Bundle            `json:"bundle"`
	Digest     string            `json:"digest"`
	Base       map[string]string `json:"base"`
	CreatedAt  time.Time         `json:"created_at"`
	ExpiresAt  time.Time         `json:"expires_at"`
	TTLSeconds int               `json:"ttl_seconds"`
	RollbackOf string            `json:"rollback_of,omitempty"`
	Caller     Caller            `json:"caller"`
	Checksum   string            `json:"checksum"`
}

type planFile struct {
	Version int             `json:"version"`
	Plans   []persistedPlan `json:"plans"`
}

// checksum detects record edits; it is not an approval credential. Rebuilding
// against the live base and a new in-process approval remain mandatory.
func (p persistedPlan) checksum() string {
	p.Checksum = ""
	b, _ := json.Marshal(p)
	return sha(string(b))
}

func recordPlan(p *Plan) (persistedPlan, bool) {
	r := persistedPlan{ID: p.ID, Bundle: Bundle{Name: p.Name, TaskID: p.TaskID}, Digest: p.Digest,
		Base: map[string]string{}, CreatedAt: p.CreatedAt, ExpiresAt: p.ExpiresAt, TTLSeconds: p.TTLSeconds,
		RollbackOf: p.RollbackOf, Caller: p.PlannedBy}
	for _, f := range p.Files {
		// Rollbacks reference their existing backup rather than duplicating its
		// contents (which may contain pre-existing secret literals).
		content := f.content
		if p.RollbackOf != "" {
			content = ""
		}
		if ScanSecrets(content) != "" {
			return persistedPlan{}, false
		}
		r.Bundle.Files = append(r.Bundle.Files, File{Path: f.Path, Content: content, Delete: f.Action == "delete"})
		r.Base[f.Path] = f.baseSHA256
	}
	r.Checksum = r.checksum()
	return r, true
}

// persistLocked replaces the snapshot atomically at 0600, including on an
// existing permissive file. Approval removes the plan from disk before success.
func (e *Engine) persistLocked() error {
	if e.PlansFile == "" {
		return nil
	}
	snapshot := planFile{Version: 1, Plans: []persistedPlan{}}
	for _, p := range e.plans {
		if p.State != StatePlanned {
			continue
		}
		r, safe := recordPlan(p)
		p.SurvivesRestart = safe
		if safe {
			snapshot.Plans = append(snapshot.Plans, r)
		}
	}
	sort.Slice(snapshot.Plans, func(i, j int) bool { return snapshot.Plans[i].ID < snapshot.Plans[j].ID })
	b, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.PlansFile), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(e.PlansFile), ".catalog-plans-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(name, e.PlansFile)
}

// RestorePlans is called once during startup, before exposing the engine.
// Records are untrusted input: rebuild from the live base, compare every base
// hash and digest, and keep original expiry (including the one-hour rearm grace).
// Unknown fields, including forged approval fields, cannot grant any authority.
func (e *Engine) RestorePlans(ctx context.Context) error {
	if e.PlansFile == "" {
		return nil
	}
	b, err := os.ReadFile(e.PlansFile)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var snapshot planFile
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return fmt.Errorf("decode catalog plans: %w", err)
	}
	if snapshot.Version != 1 {
		return fmt.Errorf("unsupported catalog plans version %d", snapshot.Version)
	}
	if len(snapshot.Plans) > maxPlans {
		return fmt.Errorf("catalog plans snapshot exceeds limit %d", maxPlans)
	}
	restored := map[string]*Plan{}
	for _, r := range snapshot.Plans {
		drop := func(reason string) { slog.Warn("dropping persisted catalog plan", "plan_id", r.ID, "reason", reason) }
		if r.Checksum != r.checksum() || r.ID == "" || len(r.Base) != len(r.Bundle.Files) || r.TTLSeconds <= 0 ||
			r.TTLSeconds > int(7*24*time.Hour/time.Second) || int(r.ExpiresAt.Sub(r.CreatedAt)/time.Second) != r.TTLSeconds || r.ExpiresAt.Sub(r.CreatedAt) > 7*24*time.Hour ||
			r.CreatedAt.After(e.now()) {
			drop("record integrity check failed")
			continue
		}
		if e.now().After(r.ExpiresAt.Add(rearmGrace)) {
			continue
		}
		if _, duplicate := restored[r.ID]; duplicate {
			drop("duplicate plan id")
			continue
		}
		// Ordinary bundles pass the ordinary secret checks. Rollbacks may
		// restore existing literals only from the same validated backup reference.
		bundle, rollback, err := e.restoreBundle(r)
		if err != nil {
			drop("rollback backup reference no longer valid")
			continue
		}
		p, err := e.buildPlan(ctx, bundle, Options{Caller: r.Caller, expectedBase: r.Base}, rollback, r.RollbackOf)
		if err != nil {
			drop("bundle or live base no longer valid")
			continue
		}
		if p.Digest != r.Digest || p.State != StatePlanned {
			drop("digest or validation no longer matches")
			continue
		}
		p.ID, p.CreatedAt, p.ExpiresAt, p.TTLSeconds = r.ID, r.CreatedAt, r.ExpiresAt, r.TTLSeconds
		p.SurvivesRestart = true
		// Approval comes from the current daemon setting, never the snapshot.
		// Only fully rebuilt, still-live records may be autoapproved.
		if e.ApprovalOff && !e.now().After(p.ExpiresAt) {
			now := e.now().UTC()
			p.State, p.ApprovedAt, p.ApprovedBy, p.AllowRunning = StateApproved, &now, "approval-disabled", true
			p.ApprovalRequired = ""
			p.SurvivesRestart = false
		}
		restored[p.ID] = p
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.plans) != 0 {
		return errors.New("restore catalog plans must precede planning")
	}
	e.plans = restored
	if err := e.persistLocked(); err != nil {
		e.plans = nil
		return err
	}
	return nil
}

// checkLiveBase binds approval to the same live base as Apply. Both checks are
// necessary because the catalog can change between approval and installation.
func (e *Engine) checkLiveBase(p *Plan) error {
	if digestOf(p) != p.Digest {
		return fmt.Errorf("plan %q digest no longer matches its content", p.ID)
	}
	for _, f := range p.Files {
		if err := ValidatePath(f.Path); err != nil {
			return err
		}
		if err := checkNoSymlinks(e.Root, f.Path); err != nil {
			return err
		}
		if f.Action != "delete" && sha(f.content) != f.NewSHA256 {
			return fmt.Errorf("plan %q content integrity check failed", p.ID)
		}
		live := "-"
		b, err := os.ReadFile(filepath.Join(e.Root, filepath.FromSlash(f.Path)))
		if err == nil {
			live = sha(string(b))
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if live != f.baseSHA256 {
			return fmt.Errorf("stale plan: %s changed after the plan was made; plan again", f.Path)
		}
	}
	return nil
}

// restoreBundle resolves rollback references without trusting any disk-supplied
// content or security finding. The rebuilt digest also binds the backup bytes.
func (e *Engine) restoreBundle(r persistedPlan) (Bundle, bool, error) {
	if r.RollbackOf == "" {
		return r.Bundle, false, nil
	}
	if filepath.Base(r.RollbackOf) != r.RollbackOf || !strings.HasPrefix(r.RollbackOf, "catalog-pre-") {
		return Bundle{}, false, errors.New("invalid rollback backup id")
	}
	dir := filepath.Join(e.BackupRoot, r.RollbackOf)
	info, err := os.Lstat(dir)
	if err != nil {
		return Bundle{}, false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return Bundle{}, false, errors.New("rollback backup is not a real directory")
	}
	backup, err := listAllowed(dir)
	if err != nil {
		return Bundle{}, false, err
	}
	b := r.Bundle
	b.Files = append([]File(nil), b.Files...)
	for i, f := range b.Files {
		if f.Content != "" {
			return Bundle{}, false, errors.New("rollback records must use backup references")
		}
		if f.Delete {
			// A deletion references the backup's absence, just as a write
			// references its bytes. Both must still match on restart.
			if _, exists := backup[f.Path]; exists {
				return Bundle{}, false, errors.New("rollback deletion path now present in backup")
			}
			continue
		}
		content, ok := backup[f.Path]
		if !ok {
			return Bundle{}, false, errors.New("rollback file absent from backup")
		}
		b.Files[i].Content = content
	}
	return b, true, nil
}
