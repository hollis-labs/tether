//go:build linux

package shimhost

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestSubmissionAttemptPersistedBeforeCommand(t *testing.T) {
	cfg, spec := hostSpec(t)
	cfg.Backend = SystemdUser
	cfg.AllowSystemd = true
	var p *Provider
	calls := 0
	cfg.Command = func(context.Context, []string) ([]byte, error) {
		calls++
		var record map[string]any
		if e := ReadPrivateJSON(filepath.Join(p.SessionDir(spec.Session), "placement.json"), 1<<20, &record); e != nil {
			t.Fatal(e)
		}
		if id, ok := record["submission_attempt_id"].(string); !ok || len(id) != 64 {
			t.Fatalf("submit without durable attempt identity: %+v", record)
		}
		return nil, errors.New("uncertain submit")
	}
	var e error
	p, e = New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	_, e = p.Place(context.Background(), "submission", spec)
	if e == nil || calls != 1 {
		t.Fatalf("submission: %v calls%d", e, calls)
	}
}

func uncertainAttempt(t *testing.T) (*Provider, Receipt, Config, int) {
	t.Helper()
	cfg, spec := hostSpec(t)
	cfg.Backend = SystemdUser
	cfg.AllowSystemd = true
	calls := 0
	cfg.Command = func(context.Context, []string) ([]byte, error) { calls++; return nil, errors.New("uncertain submit") }
	p, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	r, e := p.Place(context.Background(), "submission", spec)
	if e == nil || !r.Attempted || r.SubmissionAttemptID == "" {
		t.Fatalf("attempt fixture: %+v %v", r, e)
	}
	return p, r, cfg, calls
}

func TestFenceSurvivesReleaseAndBlocksQueuedRetry(t *testing.T) {
	p, r, cfg, _ := uncertainAttempt(t)
	f, e := p.AcquireRetirementFence(context.Background(), r, "retirement")
	if e != nil {
		t.Fatal(e)
	}
	if f.ProofCapability() == nil {
		t.Fatal("fence manufactured absence")
	}
	desc, e := Descriptor(r)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { _, e := p.Place(ctx, "submission", desc); done <- e }()
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("queued submission passed fence")
		}
	case <-ctx.Done():
		t.Fatal("queued submission exceeded deadline")
	}
	// A new provider instance models loss of in-memory state at daemon crash.
	again, e := New(cfg)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = again.Place(context.Background(), "new-key", desc); e == nil {
		t.Fatal("crash retry passed persistent fence")
	}
	var saved Receipt
	if e = ReadPrivateJSON(metadataPath(r), 1<<20, &saved); e != nil {
		t.Fatal(e)
	}
	if saved.Retired || saved.SubmissionAttemptID != r.SubmissionAttemptID {
		t.Fatal("fence changed canonical retirement/attempt facts")
	}
	if _, e = Descriptor(r); e != nil {
		t.Fatal("unknown placement lost descriptor")
	}
}

func TestFinalSubmitCheckRefusesChangedIntent(t *testing.T) {
	for _, change := range []string{"retired", "attempt", "fence", "malformed"} {
		t.Run(change, func(t *testing.T) {
			p, r, _, _ := uncertainAttempt(t)
			saved := r
			switch change {
			case "retired":
				saved.Retired = true
			case "attempt":
				saved.SubmissionAttemptID = "changed"
			case "fence":
				if e := WritePrivateJSON(filepath.Join(p.SessionDir(r.Session), "retirement-fence.json"), []string{"unknown"}); e != nil {
					t.Fatal(e)
				}
			case "malformed":
				if e := WritePrivateJSON(metadataPath(r), []string{"unknown"}); e != nil {
					t.Fatal(e)
				}
			}
			if change == "retired" || change == "attempt" {
				if e := WritePrivateJSON(metadataPath(r), saved); e != nil {
					t.Fatal(e)
				}
			}
			if e := p.verifySubmissionIntent(p.SessionDir(r.Session), r); e == nil {
				t.Fatal("changed final intent authorized submit")
			}
		})
	}
}

func TestChangedFenceRevisionRefused(t *testing.T) {
	p, r, _, _ := uncertainAttempt(t)
	f, e := p.AcquireRetirementFence(context.Background(), r, "retire")
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	changed := r
	changed.Fingerprint = "changed"
	if e = WritePrivateJSON(metadataPath(r), changed); e != nil {
		t.Fatal(e)
	}
	if f, e = p.AcquireRetirementFence(context.Background(), r, "retire"); e == nil {
		_ = f.Close()
		t.Fatal("changed receipt passed old fence")
	}
	if _, e = Descriptor(r); e != nil {
		t.Fatal("refusal lost descriptor")
	}
}

func TestRetirementFenceBlocksActualSubmit(t *testing.T) {
	for _, shape := range []string{"valid", "malformed"} {
		t.Run(shape, func(t *testing.T) {
			cfg, spec := hostSpec(t)
			cfg.Backend = SystemdUser
			cfg.AllowSystemd = true
			calls := 0
			cfg.Command = func(context.Context, []string) ([]byte, error) { calls++; return nil, errors.New("submit") }
			p, e := New(cfg)
			if e != nil {
				t.Fatal(e)
			}
			dir := p.SessionDir(spec.Session)
			if e = PrivateDir(dir); e != nil {
				t.Fatal(e)
			}
			var fence any = map[string]any{"version": "shim.submission-fence.v1", "retirement_operation": "retire", "session": spec.Session, "instance": spec.Instance, "generation": spec.Generation}
			if shape == "malformed" {
				fence = []string{"invalid"}
			}
			if e = WritePrivateJSON(filepath.Join(dir, "retirement-fence.json"), fence); e != nil {
				t.Fatal(e)
			}
			r, e := p.Place(context.Background(), "submission", spec)
			if e == nil || calls != 0 || r.Attempted {
				raw, _ := json.Marshal(r)
				t.Fatalf("fenced submission accepted: %s %v calls%d", raw, e, calls)
			}
		})
	}
}
