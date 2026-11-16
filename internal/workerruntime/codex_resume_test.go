package workerruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/hollis-labs/libs/harness/core/shim"
	mesh "github.com/hollis-labs/libs/mesh/core"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
)

func retiredTestPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func codexSourceFixture(t *testing.T) (*Executor, workerwire.LaunchSpec, string) {
	t.Helper()
	e := NewExecutor(t.TempDir(), "unused", nil)
	id := "old"
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(e.Root, "hosts", hex.EncodeToString(sum[:8]))
	cwd := filepath.Join(e.Root, "workspaces", id, "workroot")
	home := filepath.Join(e.Root, "workspaces", id, "boot", "native")
	for _, p := range []string{dir, cwd, filepath.Join(home, "sessions")} {
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	d := shim.Launch{Session: id, BootGeneration: "generation-1", Actor: mesh.Actor{URN: "urn:agent:test"}, Env: []string{"CODEX_HOME=" + home}}
	descriptor := filepath.Join(dir, "launch.json")
	if err := shimhost.WritePrivateJSON(descriptor, d); err != nil {
		t.Fatal(err)
	}
	if err := shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), execution{PID: retiredTestPID(t), ID: id, Provider: "codex", State: "exited", Descriptor: descriptor, Workspace: cwd}); err != nil {
		t.Fatal(err)
	}
	rec := outputRecord{Frame: workerwire.Frame{Stream: "stdout", Data: []byte("{\"id\":2,\"result\":{\"thread\":{\"id\":\"thread-old\"}}}\n")}}
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(dir, "output.jsonl"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	return e, workerwire.LaunchSpec{Session: "new", Actor: "urn:agent:test", Provider: "codex", RuntimeKind: "jsonrpc-stdio", ResumeSourceSessionID: id, ResumeGeneration: "generation-1", ResumeSessionID: "thread-old"}, dir
}

func TestCodexWorkerSourceFence(t *testing.T) {
	for _, tc := range []string{"success", "stale-generation", "wrong-thread", "wrong-actor", "live-source", "unknown-process", "host-only-process", "missing-transcript", "changed-cwd"} {
		t.Run(tc, func(t *testing.T) {
			e, spec, dir := codexSourceFixture(t)
			before, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
			switch tc {
			case "stale-generation":
				spec.ResumeGeneration = "old"
			case "wrong-thread":
				spec.ResumeSessionID = "other"
			case "wrong-actor":
				spec.Actor = "other"
			case "missing-transcript":
				_ = os.Remove(filepath.Join(dir, "output.jsonl"))
			case "live-source":
				var v execution
				_ = json.Unmarshal(before, &v)
				v.PID = os.Getpid()
				_ = shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), &v)
			case "unknown-process":
				var v execution
				_ = json.Unmarshal(before, &v)
				v.PID, v.HostPID = 0, 0
				_ = shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), &v)
			case "host-only-process":
				var v execution
				_ = json.Unmarshal(before, &v)
				v.HostPID = v.PID
				_ = shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), &v)
			case "changed-cwd":
				var v execution
				_ = json.Unmarshal(before, &v)
				v.Workspace = t.TempDir()
				_ = shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), &v)
			}
			source, release, err := e.reserveCodexSource(spec)
			if tc != "success" {
				if !errors.Is(err, ErrResumeUnavailable) {
					t.Fatalf("expected refusal, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if source.Thread != "thread-old" || source.Generation != "generation-1" {
				t.Fatalf("bad receipt %+v", source)
			}
			if _, _, err := e.reserveCodexSource(spec); !errors.Is(err, ErrResumeUnavailable) {
				t.Fatalf("duplicate writer admitted: %v", err)
			}
			after, _ := os.ReadFile(filepath.Join(dir, "meta.json"))
			if string(before) != string(after) {
				t.Fatal("source metadata changed")
			}
			release()
			if _, release2, err := e.reserveCodexSource(spec); err != nil {
				t.Fatal(err)
			} else {
				release2()
			}
		})
	}
}
func TestCodexWorkerConcurrentSourceReservation(t *testing.T) {
	e, spec, _ := codexSourceFixture(t)
	var wg sync.WaitGroup
	won := make(chan func(), 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, release, err := e.reserveCodexSource(spec)
			if err == nil {
				won <- release
			}
		}()
	}
	wg.Wait()
	close(won)
	n := 0
	for release := range won {
		n++
		release()
	}
	if n != 1 {
		t.Fatalf("writers=%d", n)
	}
}

type nativeRPCFixture struct {
	methods  []string
	response string
	failure  bool
}

func (f *nativeRPCFixture) Call(_ context.Context, method string, params any) (json.RawMessage, error) {
	f.methods = append(f.methods, method)
	if method == "thread/resume" {
		m := params.(map[string]any)
		if m["threadId"] != "thread-old" || m["cwd"] != "/work" || m["excludeTurns"] != true {
			return nil, errors.New("wrong resume request")
		}
		if f.failure {
			return nil, errors.New("missing native thread")
		}
		return json.RawMessage(f.response), nil
	}
	return json.RawMessage(`{}`), nil
}
func TestCodexNativeReadinessNeverStartsFreshOrTurns(t *testing.T) {
	for _, tc := range []struct {
		name, response   string
		failure, wantErr bool
	}{{"retained", `{"thread":{"id":"thread-old"}}`, false, false}, {"missing", `{}`, true, true}, {"changed", `{"thread":{"id":"other"}}`, false, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rpc := &nativeRPCFixture{response: tc.response, failure: tc.failure}
			err := resumeCodexRPC(context.Background(), rpc, "thread-old", "/work")
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v", err)
			}
			if !reflect.DeepEqual(rpc.methods, []string{"initialize", "thread/resume"}) {
				t.Fatalf("unexpected methods %v", rpc.methods)
			}
		})
	}
}

func TestCodexWorkerReclaimsOnlyRetiredUnadmittedAttempt(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(fmt.Sprint(admitted), func(t *testing.T) {
			e, spec, _ := codexSourceFixture(t)
			_, _, err := e.reserveCodexSource(spec)
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256([]byte(spec.Session))
			dir := filepath.Join(e.Root, "hosts", hex.EncodeToString(sum[:8]))
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			old := &execution{PID: retiredTestPID(t), ID: spec.Session, Provider: "codex", State: "exited"}
			if admitted {
				old.NativeContext = &workerwire.NativeContext{Thread: spec.ResumeSessionID}
			}
			if err := shimhost.WritePrivateJSON(filepath.Join(dir, "meta.json"), old); err != nil {
				t.Fatal(err)
			}
			spec.Session = "retry"
			_, release, err := e.reserveCodexSource(spec)
			if admitted {
				if err == nil {
					release()
					t.Fatal("reclaimed successful writer")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				release()
			}
		})
	}
}
