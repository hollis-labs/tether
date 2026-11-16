package workerruntime

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/libs/harness/core/shim"
	"github.com/hollis-labs/tether/internal/shimhost"
	"github.com/hollis-labs/tether/internal/workerwire"
)

type codexSource struct {
	workerwire.NativeContext
	Home string
}

// Probe reads only retained worker-owned evidence. Terminal hub state alone is
// insufficient: the source provider must be gone; a retained journal shim may remain.
func (e *Executor) codexSource(id string) (codexSource, error) {
	var out codexSource
	if !providerSessionID.MatchString(id) {
		return out, resumeUnavailable("invalid source session")
	}
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(e.Root, "hosts", hex.EncodeToString(sum[:8]))
	var old execution
	if err := shimhost.ReadPrivateJSON(filepath.Join(dir, "meta.json"), 1<<20, &old); err != nil {
		return out, resumeUnavailable("source execution receipt unavailable")
	}
	if old.ID != id || old.Provider != "codex" || old.Descriptor != filepath.Join(dir, "launch.json") || (old.State != "exited" && old.State != "failed" && old.State != "completed" && old.State != "killed") {
		return out, resumeUnavailable("source execution is not terminal Codex")
	}
	// A retained shim may still serve its journal after the child exited. It is
	// not a native writer; require a terminal child receipt and a dead child PID.
	if !retiredNativeProcess(old.PID, old.HostPID) {
		return out, resumeUnavailable("source process custody is not retired")
	}

	d, err := shim.ReadLaunch(old.Descriptor)
	if err != nil || d.Session != id || d.BootGeneration == "" {
		return out, resumeUnavailable("source descriptor unavailable")
	}
	var home string
	for _, v := range d.Env {
		if x, ok := strings.CutPrefix(v, "CODEX_HOME="); ok {
			home = x
		}
	}
	wsRoot := e.Config.WorkspaceRoot
	if wsRoot == "" {
		wsRoot = filepath.Join(e.Root, "workspaces")
	}
	for _, p := range []string{old.Workspace, home} {
		canonical, err := filepath.EvalSymlinks(p)
		rel, re := filepath.Rel(wsRoot, p)
		if err != nil || re != nil || canonical != p || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return out, resumeUnavailable("source context path unavailable or outside worker workspaces")
		}
	}
	thread, err := sourceThread(filepath.Join(dir, "output.jsonl"))
	if err != nil {
		return out, err
	}
	out.NativeContext = workerwire.NativeContext{Session: id, Generation: d.BootGeneration, Thread: thread, Workroot: old.Workspace, Actor: string(d.Actor.URN)}
	out.Home = home
	return out, nil
}

// Source stdout is already a durable, session-owned transcript. Recover only a
// single unambiguous thread receipt, including legacy app-server RPC responses.
func sourceThread(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: caller supplies the retained transcript in the worker-owned hashed host directory.
	if err != nil {
		return "", resumeUnavailable("source native mapping unavailable")
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 64<<20 {
		return "", resumeUnavailable("source transcript exceeds bounded mapping probe")
	}
	sc := bufio.NewScanner(io.LimitReader(f, 64<<20))
	sc.Buffer(make([]byte, 4096), 4<<20)
	var stdout bytes.Buffer
	for sc.Scan() {
		var r outputRecord
		if json.Unmarshal(sc.Bytes(), &r) != nil {
			return "", resumeUnavailable("source output damaged")
		}
		if r.Frame.Stream == "stdout" || r.Frame.Stream == "" {
			stdout.Write(r.Frame.Data)
		}
	}
	if sc.Err() != nil {
		return "", resumeUnavailable("source output unreadable")
	}
	lines := bufio.NewScanner(&stdout)
	lines.Buffer(make([]byte, 4096), 4<<20)
	id := ""
	for lines.Scan() {
		var v struct {
			Method string `json:"method"`
			Result struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			} `json:"result"`
			Params struct {
				Thread struct {
					ID string `json:"id"`
				} `json:"thread"`
			} `json:"params"`
		}
		if json.Unmarshal(lines.Bytes(), &v) != nil {
			continue
		}
		found := v.Result.Thread.ID
		if v.Method == "thread/started" {
			found = v.Params.Thread.ID
		}
		if found == "" {
			continue
		}
		if !providerSessionID.MatchString(found) || id != "" && id != found {
			return "", resumeUnavailable("source thread mapping ambiguous")
		}
		id = found
	}
	if lines.Err() != nil || id == "" {
		return "", resumeUnavailable("source native mapping unavailable")
	}
	return id, nil
}

// Reserve the exact source generation once. Successful and uncertain attempts
// retain the claim across worker reconnects; a stale request cannot fork a second
// writer. Only a confirmed pre-start failure releases it.
func (e *Executor) reserveCodexSource(spec workerwire.LaunchSpec) (codexSource, func(), error) {
	source, err := e.codexSource(spec.ResumeSourceSessionID)
	if err != nil {
		return source, nil, err
	}
	if spec.Provider != "codex" || spec.RuntimeKind != "jsonrpc-stdio" || source.Generation != spec.ResumeGeneration || source.Thread != spec.ResumeSessionID || source.Actor != spec.Actor {
		return source, nil, resumeUnavailable("source generation, native thread or actor changed")
	}
	dir := filepath.Join(e.Root, "native-continuations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return source, nil, err
	}
	// Serialize reclamation as well as creation across executor processes.
	lock, err := os.OpenFile(filepath.Join(dir, "admission.lock"), os.O_CREATE|os.O_RDWR, 0600) //nolint:gosec // G304: fixed filename under the configured worker state root.
	if err != nil {
		return source, nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil { //nolint:gosec // G115: Unix open returns an int file descriptor.
		return source, nil, err
	}
	defer func() { _ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) }() //nolint:gosec // G115: Unix open returns an int file descriptor.
	sum := sha256.Sum256([]byte(source.Session + ":" + source.Generation))
	path := filepath.Join(dir, hex.EncodeToString(sum[:]))
	if owner, err := os.ReadFile(path); err == nil && e.failedNativeAttemptRetired(string(owner)) { //nolint:gosec // G304: hashed reservation filename under the worker state root.
		if err := os.Remove(path); err != nil {
			return source, nil, err
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600) //nolint:gosec // G304: hashed reservation filename under the worker state root.
	if err != nil {
		return source, nil, resumeUnavailable("source already reserved; reconcile its attempt before retry")
	}
	_, err = f.WriteString(spec.Session)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return source, nil, err
	}
	return source, func() {
		guard, err := os.OpenFile(filepath.Join(dir, "admission.lock"), os.O_RDWR, 0600) //nolint:gosec // G304: fixed filename under the worker state root.
		if err != nil {
			return
		}
		defer guard.Close()
		if syscall.Flock(int(guard.Fd()), syscall.LOCK_EX) != nil { //nolint:gosec // G115: Unix open returns an int file descriptor.
			return
		}
		defer func() { _ = syscall.Flock(int(guard.Fd()), syscall.LOCK_UN) }()            //nolint:gosec // G115: Unix open returns an int file descriptor.
		if owner, err := os.ReadFile(path); err == nil && string(owner) == spec.Session { //nolint:gosec // G304: hashed reservation filename under the worker state root.
			_ = os.Remove(path)
		}
	}, nil
}

type nativeReady struct {
	Thread   string `json:"thread"`
	Workroot string `json:"workroot"`
	Error    string `json:"error,omitempty"`
}

func waitNativeReady(ctx context.Context, path string, thread, workroot string) error {
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		var r nativeReady
		if err := shimhost.ReadPrivateJSON(path, 4096, &r); err == nil {
			if r.Error != "" {
				return resumeUnavailable("provider rejected native continuation")
			}
			if r.Thread != thread || r.Workroot != workroot {
				return resumeUnavailable("native readiness receipt mismatch")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("native readiness timeout")
		case <-tick.C:
		}
	}
}

func retiredNativeProcess(pid, hostPID int) bool {
	if pid > 1 {
		return errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	}
	return hostPID <= 1 || errors.Is(syscall.Kill(hostPID, 0), syscall.ESRCH)
}

func (e *Executor) awaitNativeRetirement(s *execution) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		terminal := s.State == "exited" || s.State == "failed" || s.State == "completed" || s.State == "killed"
		pid, host := s.PID, s.HostPID
		s.mu.Unlock()
		if terminal && retiredNativeProcess(pid, host) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

// A delayed failure may settle after its launch response. Reclaim only a
// retained failed attempt that never published native admission and whose child
// is definitely gone. Successful writers always keep their source claim.
func (e *Executor) failedNativeAttemptRetired(id string) bool {
	if !providerSessionID.MatchString(id) {
		return false
	}
	sum := sha256.Sum256([]byte(id))
	dir := filepath.Join(e.Root, "hosts", hex.EncodeToString(sum[:8]))
	var attempt execution
	if err := shimhost.ReadPrivateJSON(filepath.Join(dir, "meta.json"), 1<<20, &attempt); err != nil {
		return false
	}
	terminal := attempt.State == "exited" || attempt.State == "failed" || attempt.State == "completed" || attempt.State == "killed"
	return attempt.ID == id && attempt.Provider == "codex" && attempt.NativeContext == nil && terminal && retiredNativeProcess(attempt.PID, attempt.HostPID)
}
