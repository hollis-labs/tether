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
		// Rollbacks may restore pre-existing literals; do not copy those to records.
		if ScanSecrets(f.content) != "" {
			return persistedPlan{}, false
		}
		r.Bundle.Files = append(r.Bundle.Files, File{Path: f.Path, Content: f.content, Delete: f.Action == "delete"})
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
		// Use the ordinary input and secret checks, even for persisted rollbacks.
		p, err := e.buildPlan(ctx, r.Bundle, Options{Caller: r.Caller, expectedBase: r.Base}, false, r.RollbackOf)
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
