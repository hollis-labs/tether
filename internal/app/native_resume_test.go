package app

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hollis-labs/agentkit/agentsessions"
	"github.com/hollis-labs/go-providers/provider"
	"github.com/hollis-labs/tether/internal/launch"
	"github.com/hollis-labs/tether/internal/store"
)

type recoveryFakeRuntime struct {
	agentsessions.Runtime
	start func(agentsessions.StartOptions) (agentsessions.Session, error)
}

func (r recoveryFakeRuntime) Caps() agentsessions.Capabilities {
	return agentsessions.Capabilities{ProviderSessionID: true}
}
func (r recoveryFakeRuntime) Start(_ context.Context, o agentsessions.StartOptions) (agentsessions.Session, error) {
	return r.start(o)
}

type recoveryFakeSession struct {
	agentsessions.Session
	done    chan struct{}
	once    sync.Once
	send    func() error
	stopErr error
}

func (s *recoveryFakeSession) Wait() (int, error) { <-s.done; return 1, nil }
func (s *recoveryFakeSession) Health() agentsessions.HealthStatus {
	return agentsessions.HealthStatus{Alive: true}
}
func (s *recoveryFakeSession) Stop(context.Context) error {
	if s.stopErr != nil {
		return s.stopErr
	}
	s.once.Do(func() { close(s.done) })
	return nil
}
func (s *recoveryFakeSession) SendInput(context.Context, []byte) error { return s.send() }

func recoveryRuntimeRig(t *testing.T, start func(agentsessions.StartOptions) (agentsessions.Session, error)) *recoveryRuntime {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	plan := &launch.Plan{ProviderID: "custom-codex", ProviderBrand: "codex", ResumeSourceSessionID: "source", ResumeProviderSessionID: "thread-old", BootPrompt: "Recovery pack: interrupted turn, do not replay"}
	for _, id := range []string{"source", "resumed"} {
		if err := db.CreateSession(store.SessionRow{ID: id, LaunchID: "launch", LogicalAgentID: "agent", ProviderID: plan.ProviderID, Workspace: t.TempDir(), State: "created"}, plan); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.UpsertSessionProviderMapping("source", "tether", plan.ProviderID, "thread-old"); err != nil {
		t.Fatal(err)
	}
	return &recoveryRuntime{Runtime: recoveryFakeRuntime{start: start}, service: &Service{Store: db, resumeGrace: 25 * time.Millisecond}, plan: plan, id: "resumed", request: context.Background()}
}

func TestNativeRecoveryLostIDColdBootKeepsCanonicalSession(t *testing.T) {
	var attempts []string
	cold := &recoveryFakeSession{done: make(chan struct{}), send: func() error { return nil }}
	t.Cleanup(func() { _ = cold.Stop(context.Background()) })
	r := recoveryRuntimeRig(t, func(o agentsessions.StartOptions) (agentsessions.Session, error) {
		attempts = append(attempts, o.SessionIDPreset)
		if o.SessionIDPreset != "" {
			return nil, &agentsessions.SessionLostError{RequestedID: o.SessionIDPreset, Err: errors.New("gone")}
		}
		return cold, nil
	})
	sess, err := r.Start(context.Background(), agentsessions.StartOptions{SessionIDPreset: "thread-old"})
	if err != nil || sess != cold {
		t.Fatalf("cold start=%v %v", sess, err)
	}
	if len(attempts) != 2 || attempts[0] != "thread-old" || attempts[1] != "" {
		t.Fatalf("attempts=%v", attempts)
	}
	got, _ := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
	if got.NativeSessionID.Valid {
		t.Fatalf("stale mapping=%+v", got)
	}
	plan, _ := r.service.Store.GetLaunchPlan("resumed")
	if plan.ResumeProviderSessionID != "" || plan.ResumeSourceSessionID != "source" {
		t.Fatalf("persisted fallback=%+v", plan)
	}
}

func TestNativeRecoveryFastDeathAndTeardownFence(t *testing.T) {
	for _, unconfirmed := range []bool{false, true} {
		t.Run(map[bool]string{false: "cold", true: "no-duplicate"}[unconfirmed], func(t *testing.T) {
			attempts := 0
			failed := &recoveryFakeSession{done: make(chan struct{}), send: func() error {
				return &agentsessions.SessionLostError{RequestedID: "thread-old", Err: errors.New("gone")}
			}}
			if unconfirmed {
				failed.stopErr = errors.New("stop unavailable")
			}
			cold := &recoveryFakeSession{done: make(chan struct{}), send: func() error { return nil }}
			t.Cleanup(func() {
				failed.stopErr = nil
				_ = failed.Stop(context.Background())
				_ = cold.Stop(context.Background())
			})
			r := recoveryRuntimeRig(t, func(agentsessions.StartOptions) (agentsessions.Session, error) {
				attempts++
				if attempts == 1 {
					return failed, nil
				}
				return cold, nil
			})
			_, err := r.Start(context.Background(), agentsessions.StartOptions{SessionIDPreset: "thread-old"})
			if unconfirmed {
				if err == nil || attempts != 1 {
					t.Fatalf("unconfirmed teardown duplicated: %d %v", attempts, err)
				}
			} else if err != nil || attempts != 2 {
				t.Fatalf("fallback=%d %v", attempts, err)
			}
		})
	}
}

func TestNativeRecoveryAuthFailurePreservesID(t *testing.T) {
	attempts := 0
	r := recoveryRuntimeRig(t, func(agentsessions.StartOptions) (agentsessions.Session, error) {
		attempts++
		return nil, provider.ErrProviderNotAuthenticated
	})
	_, err := r.Start(context.Background(), agentsessions.StartOptions{SessionIDPreset: "thread-old"})
	if !errors.Is(err, provider.ErrProviderNotAuthenticated) || attempts != 1 {
		t.Fatalf("auth=%d %v", attempts, err)
	}
	got, _ := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
	if got.NativeSessionID.String != "thread-old" {
		t.Fatal("authentication failure erased native history")
	}
}

func TestNativeRecoveryFastProcessDeathRetriesOnce(t *testing.T) {
	failed := &recoveryFakeSession{done: make(chan struct{}), send: func() error { return nil }}
	close(failed.done)
	cold := &recoveryFakeSession{done: make(chan struct{}), send: func() error { return nil }}
	attempts := 0
	r := recoveryRuntimeRig(t, func(agentsessions.StartOptions) (agentsessions.Session, error) {
		attempts++
		if attempts == 1 {
			return failed, nil
		}
		return cold, nil
	})
	r.plan.ProviderBrand = "antigravity"
	t.Cleanup(func() { _ = cold.Stop(context.Background()) })
	_, err := r.Start(context.Background(), agentsessions.StartOptions{SessionIDPreset: "conversation-old"})
	if err != nil || attempts != 2 {
		t.Fatalf("fast death recovery=%d %v", attempts, err)
	}
}

func TestNativeRecoveryHealthyIDDoesNotColdRetry(t *testing.T) {
	healthy := &recoveryFakeSession{done: make(chan struct{}), send: func() error { return nil }}
	t.Cleanup(func() { _ = healthy.Stop(context.Background()) })
	attempts := 0
	r := recoveryRuntimeRig(t, func(agentsessions.StartOptions) (agentsessions.Session, error) { attempts++; return healthy, nil })
	_, err := r.Start(context.Background(), agentsessions.StartOptions{SessionIDPreset: "thread-old"})
	if err != nil || attempts != 1 {
		t.Fatalf("healthy recovery=%d %v", attempts, err)
	}
	got, _ := r.service.Store.GetSessionProviderMapping("source", "tether", r.plan.ProviderID)
	if got.NativeSessionID.String != "thread-old" {
		t.Fatal("healthy native history was erased")
	}
}
