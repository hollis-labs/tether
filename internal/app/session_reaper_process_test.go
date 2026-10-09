//go:build linux

package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
	"time"

	"github.com/hollis-labs/substrate/harness/adapters/agentsessions"
	"github.com/hollis-labs/tether/internal/store"
	"golang.org/x/sys/unix"
)

func TestReaperProcessChild(t *testing.T) {
	mode := os.Getenv("TETHER_REAPER_TEST_MODE")
	if mode == "" {
		return
	}
	sig := make(chan os.Signal, 1)
	switch mode {
	case "request_stop":
		signal.Notify(sig, syscall.SIGINT)
	case "terminate":
		signal.Ignore(syscall.SIGINT)
		signal.Notify(sig, syscall.SIGTERM)
	case "kill":
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
	default:
		os.Exit(9)
	}
	if err := os.WriteFile(os.Getenv("TETHER_REAPER_TEST_READY"), nil, 0600); err != nil {
		os.Exit(8)
	}
	if mode == "kill" {
		for {
			time.Sleep(time.Hour)
		}
	}
	<-sig
	os.Exit(0)
}

type reaperProcessRuntime struct{ command *exec.Cmd }

func (r reaperProcessRuntime) ID() string                       { return "reaper-process-test" }
func (r reaperProcessRuntime) Kind() string                     { return "cli" }
func (r reaperProcessRuntime) Caps() agentsessions.Capabilities { return agentsessions.Capabilities{} }
func (r reaperProcessRuntime) Prepare(context.Context) error    { return nil }
func (r reaperProcessRuntime) Start(context.Context, agentsessions.StartOptions) (agentsessions.Session, error) {
	if err := r.command.Start(); err != nil {
		return nil, err
	}
	return &reaperProcessSession{r.command}, nil
}

type reaperProcessSession struct{ command *exec.Cmd }

func (s *reaperProcessSession) Wait() (int, error) {
	err := s.command.Wait()
	return s.command.ProcessState.ExitCode(), err
}
func (s *reaperProcessSession) Stop(context.Context) error { return s.command.Process.Kill() }
func (*reaperProcessSession) SendInput(context.Context, []byte) error {
	return agentsessions.ErrNoInputChannel
}
func (*reaperProcessSession) Resize(context.Context, uint16, uint16) error { return nil }
func (s *reaperProcessSession) Health() agentsessions.HealthStatus {
	return agentsessions.HealthStatus{PID: s.command.Process.Pid, Alive: true}
}
func (*reaperProcessSession) CheckpointHints() (agentsessions.CheckpointHint, bool) {
	return agentsessions.CheckpointHint{}, false
}

func TestReaperDirectStopTiers(t *testing.T) {
	for _, mode := range []string{"request_stop", "terminate", "kill"} {
		t.Run(mode, func(t *testing.T) {
			svc := stopHarness(t)
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ready := filepath.Join(t.TempDir(), "ready")
			command := exec.Command(exe, "-test.run=^TestReaperProcessChild$") //nolint:gosec // current test executable, fixed argument
			command.Env = append(os.Environ(), "TETHER_REAPER_TEST_MODE="+mode, "TETHER_REAPER_TEST_READY="+ready)
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			startSession(t, svc, "tiers", reaperProcessRuntime{command})
			t.Cleanup(func() { _ = svc.Manager.Stop(context.Background(), "tiers") })
			deadline := time.Now().Add(2 * time.Second)
			for {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("child not ready")
				}
				time.Sleep(time.Millisecond)
			}
			fd, err := unix.PidfdOpen(command.Process.Pid, 0)
			if err != nil {
				t.Skipf("pidfd unavailable: %v", err)
			}
			err = unix.PidfdSendSignal(fd, 0, nil, unix.PIDFD_SIGNAL_PROCESS_GROUP)
			_ = unix.Close(fd)
			if errors.Is(err, syscall.EINVAL) {
				t.Skip("kernel lacks pidfd process-group signaling")
			}
			if err != nil {
				t.Fatal(err)
			}
			// A race-instrumented child sleeps for one second during os.Exit.
			// Allow its real process exit before asserting that no later tier ran.
			p := map[string]any{"lifecycle": map[string]string{"request_grace": "2s", "terminate_grace": "2s", "kill_grace": "3s"}}
			raw, _ := json.Marshal(p)
			if _, err := svc.Store.DB().Exec(`UPDATE launch_plans SET plan_json=? WHERE session_id=?`, string(raw), "tiers"); err != nil {
				t.Fatal(err)
			}
			if err := svc.StopSession("tiers"); err != nil {
				t.Fatal(err)
			}

			evs, err := svc.Store.QueryEvents(store.EventFilter{SessionID: "tiers", Kinds: []string{sessionReaperEvent}})
			if err != nil {
				t.Fatal(err)
			}
			var stages []string
			for i := len(evs) - 1; i >= 0; i-- {
				var p map[string]string
				if err := json.Unmarshal([]byte(evs[i].PayloadJSON), &p); err != nil {
					t.Fatal(err)
				}
				if p["stage"] != "requested" && p["stage"] != "outcome" {
					stages = append(stages, p["stage"])
				}
			}
			want := []string{"request_stop"}
			if mode != "request_stop" {
				want = append(want, "terminate")
			}
			if mode == "kill" {
				want = append(want, "kill")
			}
			if !reflect.DeepEqual(stages, want) {
				t.Fatalf("stages=%v want=%v", stages, want)
			}
			row, err := svc.Store.GetSession("tiers")
			if err != nil {
				t.Fatal(err)
			}
			if row.State != "killed" {
				t.Fatalf("outcome=%s", row.State)
			}
		})
	}
}
