package sshenroll

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/identity"
)

var targetPattern = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]*(?:@[a-zA-Z0-9_][a-zA-Z0-9_.-]*)?$`)

func validTarget(target string) bool { return len(target) <= 255 && targetPattern.MatchString(target) }
func validOperation(id string) bool {
	v, e := uuid.Parse(id)
	return e == nil && v.Version() == 4 && v.String() == id
}
func shellQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

// SSH owns only commands and a short-lived forward started by this invocation.
// stdout is a bounded private return channel; stderr/argv never become reports.
type SSH struct {
	Target, Command string
	StepTimeout     time.Duration
}

func (s *SSH) args() ([]string, error) {
	if !validTarget(s.Target) {
		return nil, problem("ssh", "invalid-target", "Use a plain host alias or user@host, without SSH command options.")
	}
	return []string{"-o", "BatchMode=yes", "-o", "ExitOnForwardFailure=yes", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "-o", "ForkAfterAuthentication=no", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "ConnectTimeout=10"}, nil
}
func (s *SSH) command() string {
	if s.Command != "" {
		return s.Command
	}
	return "ssh"
}

type privateOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *privateOutput) Write(p []byte) (int, error) {
	n := len(p)
	left := (64 << 10) - b.Len()
	if len(p) > left {
		b.overflow = true
		p = p[:left]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}
func (s *SSH) run(ctx context.Context, script string, input io.Reader, timeout time.Duration) ([]byte, error) {
	args, err := s.args()
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	args = append(args, s.Target, "sh -lc "+shellQuote(script))
	cmd := exec.CommandContext(ctx, s.command(), args...) //nolint:gosec // explicit SSH executable; validated target and quoted authored script
	cmd.Stdin = input
	output := &privateOutput{}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, problem("ssh", "command-refused", "SSH or the worker command failed; private output was withheld.")
	}
	if output.overflow {
		return nil, problem("ssh", "output-limit", "The private worker response exceeded 64 KiB.")
	}
	return output.Bytes(), nil
}

func stagePath(id string) string { return `"$HOME/.tether/enrollment/` + id + `"` }
func (s *SSH) Install(ctx context.Context, r WorkerRequest, a *Artifact) (WorkerState, error) {
	if err := r.Validate(); err != nil {
		return WorkerState{}, err
	}
	if a == nil || a.file == nil || (a.Name != "tether_"+r.Version+"_linux_amd64.tar.gz" && a.Name != "tether_"+r.Version+"_linux_arm64.tar.gz") {
		return WorkerState{}, problem("artifact", "invalid-snapshot", "Use the verified hub snapshot for this exact release.")
	}
	if !validOperation(r.OperationID) {
		return WorkerState{}, problem("install", "invalid-operation", "A retained operation UUID is required.")
	}
	stage := stagePath(r.OperationID)
	// The first writer owns this staging leaf. Fixed names are never followed
	// through symlinks; exclusive upload candidates are atomically published.
	prepare := `set -eu; umask 077; [ ! -L "$HOME/.tether" ]; mkdir -p "$HOME/.tether/enrollment"; [ ! -L "$HOME/.tether/enrollment" ]; mkdir -p ` + stage + `; [ ! -L ` + stage + ` ]; [ "$(stat -c %u ` + stage + `)" = "$(id -u)" ]; [ "$(stat -c %a ` + stage + `)" = 700 ]`
	if _, err := s.run(ctx, prepare, nil, s.StepTimeout); err != nil {
		return WorkerState{}, safeStep("stage", err)
	}
	upload := func(name string, input io.Reader) error {
		script := `set -eu; umask 077; stage=` + stage + `; [ -d "$stage" ] && [ ! -L "$stage" ]; candidate=$(mktemp "$stage/.upload-XXXXXX"); trap 'rm -f "$candidate"' EXIT; cat >"$candidate"; [ ! -L "$stage/` + name + `" ]; mv -T "$candidate" "$stage/` + name + `"`
		_, err := s.run(ctx, script, input, 5*time.Minute)
		return safeStep("upload", err)
	}
	reader, err := a.Reader()
	if err != nil {
		return WorkerState{}, safeStep("upload", err)
	}
	for _, file := range []struct {
		name  string
		input io.Reader
	}{{a.Name, reader}, {"checksums.txt", bytes.NewReader(a.Checksums())}, {"bootstrap", a.Binary()}} {
		if err := upload(file.name, file.input); err != nil {
			return WorkerState{}, err
		}
	}
	_, err = s.run(ctx, `set -eu; chmod 700 `+stage+`/bootstrap`, nil, s.StepTimeout)
	if err != nil {
		return WorkerState{}, safeStep("install", err)
	}
	return s.worker(ctx, "install", r)
}
func (s *SSH) workerOutput(ctx context.Context, action string, r WorkerRequest) ([]byte, error) {
	if !validOperation(r.OperationID) {
		return nil, problem(action, "invalid-operation", "A retained operation UUID is required.")
	}
	script := `set -eu; stage=` + stagePath(r.OperationID) + `; [ ! -L "$stage" ] && [ -f "$stage/bootstrap" ] && [ ! -L "$stage/bootstrap" ]; exec "$stage/bootstrap" env __worker ` + action
	data, err := s.run(ctx, script, jsonInput(r), 5*time.Minute)
	return data, safeStep(action, err)
}
func (s *SSH) worker(ctx context.Context, action string, r WorkerRequest) (WorkerState, error) {
	data, err := s.workerOutput(ctx, action, r)
	if err != nil {
		return WorkerState{}, err
	}
	var state WorkerState
	if json.Unmarshal(data, &state) != nil {
		return state, problem(action, "invalid-response", "The worker must return its managed-operation receipt.")
	}
	return state, nil
}
func (s *SSH) Inspect(ctx context.Context, r WorkerRequest) (WorkerState, error) {
	return s.worker(ctx, "inspect", r)
}
func (s *SSH) Grant(ctx context.Context, r WorkerRequest) (identity.IssuedGrant, error) {
	data, err := s.workerOutput(ctx, "grant", r)
	if err != nil {
		return identity.IssuedGrant{}, err
	}
	var grant identity.IssuedGrant
	if json.Unmarshal(data, &grant) != nil {
		return grant, problem("pair", "invalid-grant", "The worker returned an invalid private grant response.")
	}
	return grant, nil
}
func (s *SSH) Rollback(ctx context.Context, r WorkerRequest) error {
	_, err := s.worker(ctx, "rollback", r)
	return err
}

type sshTunnel struct {
	base, directory, socket string
	cancel                  context.CancelFunc
	done                    <-chan error
	once                    sync.Once
}

func (t *sshTunnel) BaseURL() string { return t.base }
func (t *sshTunnel) Close() error {
	t.once.Do(func() {
		t.cancel()
		<-t.done
		_ = os.Remove(t.socket)
		_ = os.Remove(t.directory)
	})
	return nil
}
func (s *SSH) Forward(ctx context.Context, remotePort int) (Tunnel, error) {
	if remotePort < 1024 || remotePort > 65535 {
		return nil, problem("forward", "invalid-port", "Choose an unprivileged loopback worker port.")
	}
	args, err := s.args()
	if err != nil {
		return nil, err
	}
	// The private directory prevents another UID from replacing the local
	// listener before readiness or after an SSH failure. A public descriptor
	// alone cannot authenticate a newly rebound TCP port.
	directory, err := os.MkdirTemp("", "tether-forward-")
	if err != nil {
		return nil, problem("forward", "local-path-unavailable", "Cannot prepare the invocation-owned private SSH forward.")
	}
	cleanup := func() { _ = os.Remove(filepath.Join(directory, "forward.sock")); _ = os.Remove(directory) }
	if err := privateDirectory(directory); err != nil {
		cleanup()
		return nil, err
	}
	socket := filepath.Join(directory, "forward.sock")
	if strings.ContainsAny(socket, ":\r\n") || len(socket) > 100 {
		cleanup()
		return nil, problem("forward", "invalid-socket-path", "Set a short absolute TMPDIR without symlink ancestors on the hub.")
	}
	ctx, cancel := context.WithCancel(ctx)
	args = append(args, "-o", "StreamLocalBindMask=0177", "-o", "StreamLocalBindUnlink=no", "-n", "-N", "-L", socket+":127.0.0.1:"+portText(remotePort), s.Target)
	cmd := exec.CommandContext(ctx, s.command(), args...) //nolint:gosec // same validated fixed SSH configuration; no credential arguments
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	cmd.WaitDelay = 200 * time.Millisecond
	if err := cmd.Start(); err != nil {
		cancel()
		cleanup()
		return nil, problem("forward", "start-failed", "SSH could not start the owned local forward.")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait(); close(done) }()
	t := &sshTunnel{base: "http://127.0.0.1:" + portText(remotePort), directory: directory, socket: socket, cancel: cancel, done: done}
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = t.Close()
			return nil, ctx.Err()
		case <-done:
			_ = t.Close()
			return nil, problem("forward", "forward-refused", "SSH exited before establishing the loopback forward.")
		case <-deadline.C:
			_ = t.Close()
			return nil, problem("forward", "forward-timeout", "SSH did not establish its private forward within 10s.")
		case <-time.After(25 * time.Millisecond):
			conn, e := net.DialTimeout("unix", socket, 100*time.Millisecond)
			if e == nil {
				_ = conn.Close()
				return t, nil
			}
		}
	}
}

var _ Remote = (*SSH)(nil)
