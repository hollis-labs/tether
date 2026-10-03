//go:build !windows

// Package shimhost places a shim independently of the controller's lifetime.
// It accepts the REAL provider's already resolved argv and complete environment;
// sandbox/limits must be applied to that argv before Place, never only to a bridge.
package shimhost

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/hollis-labs/substrate/harness/shim"
)

const DefaultJournalBytes int64 = 256 << 20
const Detached = "detached"
const SystemdUser = "systemd-user"

// Failure is secret-free. An uncertain submit must be inspected, never resent.
type Failure struct {
	Code    string
	Message string
}

func (e *Failure) Error() string  { return e.Code + ": " + e.Message }
func fail(code, msg string) error { return &Failure{Code: code, Message: msg} }

type Config struct {
	StateDir     string
	ShimCommand  []string
	HostEnv      []string
	Backend      string
	AllowSystemd bool
	JournalBytes int64
	// Command is the systemd control seam; nil uses exec.CommandContext. Detached
	// placement always executes the shim itself, not this seam.
	Command func(context.Context, []string) ([]byte, error)
}
type Provider struct{ cfg Config }

// Receipt contains paths and identity, never the capability or provider env.
type Receipt struct {
	OperationKey   string `json:"operation_key"`
	Session        string `json:"session"`
	Instance       string `json:"instance"`
	Generation     uint64 `json:"generation,string"`
	DescriptorPath string `json:"descriptor_path"`
	SocketPath     string `json:"socket_path"`
	Backend        string `json:"backend"`
	UnitName       string `json:"unit_name,omitempty"`
	Journal        string `json:"journal,omitempty"`
	Epoch          string `json:"epoch,omitempty"`
	HostPID        int    `json:"host_pid"`
	ShimPID        int    `json:"shim_pid"`
	ProviderPID    int    `json:"provider_pid"`
	Fingerprint    string `json:"fingerprint"`
	Attempted      bool   `json:"attempted"`
}
type Inspection struct {
	Receipt Receipt
	Running bool
	Gone    bool
	Exit    shim.Exit
}

func New(cfg Config) (*Provider, error) {
	if cfg.Backend == "" {
		cfg.Backend = Detached
	}
	if cfg.Backend != Detached && cfg.Backend != SystemdUser {
		return nil, fail("invalid_backend", "unsupported shim host backend")
	}
	if cfg.Backend == SystemdUser && !cfg.AllowSystemd {
		return nil, fail("backend_disabled", "systemd-user requires explicit enablement")
	}
	if len(cfg.ShimCommand) == 0 || !filepath.IsAbs(cfg.ShimCommand[0]) {
		return nil, fail("invalid_command", "shim executable must be absolute")
	}
	if cfg.JournalBytes == 0 {
		cfg.JournalBytes = DefaultJournalBytes
	}
	if cfg.JournalBytes < 2<<20 {
		return nil, fail("invalid_config", "journal cap below 2 MiB")
	}
	if err := PrivateDir(cfg.StateDir); err != nil {
		return nil, err
	}
	if cfg.Command == nil {
		cfg.Command = func(ctx context.Context, argv []string) ([]byte, error) {
			cmd := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // Host-owned systemd argv, no shell.
			return cmd.Output()
		}
	}
	return &Provider{cfg: cfg}, nil
}
func hash(s string) string                    { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:])[:16] }
func (p *Provider) dir(session string) string { return filepath.Join(p.cfg.StateDir, hash(session)) }
func (p *Provider) unit(spec shim.Launch) string {
	return "tether-shim-" + hash(spec.Instance) + "-" + hash(spec.Session) + ".service"
}
func metadataPath(r Receipt) string {
	return filepath.Join(filepath.Dir(r.DescriptorPath), "placement.json")
}

// Place commits pre-submit intent under an OS lock. Repeating a matching key
// returns the same inspected placement; an absent host after uncertain submit
// never licenses an automatic second spawn.
func (p *Provider) Place(ctx context.Context, key string, spec shim.Launch) (Receipt, error) {
	var zero Receipt
	if key == "" || len(key) > 256 || spec.Session == "" || spec.Instance == "" || spec.Generation == 0 {
		return zero, fail("invalid_request", "missing placement identity/key")
	}
	if len(spec.Argv) == 0 || !filepath.IsAbs(spec.Argv[0]) || !filepath.IsAbs(spec.Cwd) || !filepath.IsAbs(spec.PinPath) || spec.PinKey == "" || spec.BootGeneration == "" || spec.Reservation == "" || spec.Actor.Validate() != nil || spec.Subject.Validate() != nil {
		return zero, fail("invalid_request", "provider argv/cwd/identity/pin must be resolved before placement")
	}
	dir := p.dir(spec.Session)
	if err := PrivateDir(dir); err != nil {
		return zero, err
	}
	lock, err := Lock(filepath.Join(dir, "placement.lock"))
	if err != nil {
		return zero, err
	}
	defer func() { _ = lock.Close() }()
	spec.Secret = ""
	spec.ControlDir = filepath.Join(dir, "c")
	spec.JournalDir = filepath.Join(dir, "j")
	if spec.JournalBytes == 0 {
		spec.JournalBytes = p.cfg.JournalBytes
	}
	if len(filepath.Join(spec.ControlDir, "control.sock")) >= 104 {
		return zero, fail("invalid_request", "control socket path too long")
	}
	raw, err := json.Marshal(spec) //nolint:gosec // Secret cleared above; fingerprint contains no capability.
	if err != nil {
		return zero, err
	}
	h := sha256.Sum256(raw)
	fingerprint := hex.EncodeToString(h[:])
	r := Receipt{OperationKey: key, Session: spec.Session, Instance: spec.Instance, Generation: spec.Generation, DescriptorPath: filepath.Join(dir, "launch.json"), SocketPath: filepath.Join(spec.ControlDir, "control.sock"), Backend: p.cfg.Backend, Fingerprint: fingerprint}
	if r.Backend == SystemdUser {
		r.UnitName = p.unit(spec)
	}
	var old Receipt
	if err = ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &old); err == nil {
		if old.OperationKey != key || old.Fingerprint != fingerprint || old.Backend != r.Backend {
			return zero, fail("idempotency_conflict", "placement key/launch changed")
		}
		if old.Attempted {
			inspection, e := p.Inspect(ctx, old)
			if e != nil {
				return old, e
			}
			if inspection.Gone {
				return old, fail("outcome_unknown", "previous submit has no live host; native recovery is required")
			}
			return inspection.Receipt, nil
		}
		r = old
	} else if !os.IsNotExist(err) {
		return zero, err
	}
	if err := ctx.Err(); err != nil {
		return r, err
	}
	var secret [32]byte
	if _, err = rand.Read(secret[:]); err != nil {
		return r, err
	}
	spec.Secret = hex.EncodeToString(secret[:])
	raw, err = json.Marshal(spec) //nolint:gosec // Capability intentionally stored only in the private 0600 descriptor.
	if err != nil {
		return r, err
	}
	if len(raw) > shim.MaxFrame {
		return r, fail("invalid_request", "launch descriptor exceeds frame limit")
	}
	if err = WritePrivateJSON(r.DescriptorPath, spec); err != nil {
		return r, err
	}
	// Persist intent BEFORE either exec or a service-manager submission.
	r.Attempted = true
	if err = WritePrivateJSON(metadataPath(r), r); err != nil {
		return r, err
	}
	argv := append(append([]string(nil), p.cfg.ShimCommand...), "--launch", r.DescriptorPath)
	if r.Backend == Detached {
		cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // Configured shim executable and private descriptor path.
		cmd.Env = append([]string{}, p.cfg.HostEnv...)
		cmd.Dir = dir
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		// No provider output goes to this log: the shim journals its own pipes.
		logPath := filepath.Join(dir, "host.log")
		fd, e := syscall.Open(logPath, syscall.O_CREAT|syscall.O_WRONLY|syscall.O_APPEND|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
		if e != nil {
			return r, e
		}
		logFile := os.NewFile(uintptr(fd), logPath)
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		e = cmd.Start()
		_ = logFile.Close()
		if e != nil {
			return r, fail("outcome_unknown", "shim spawn did not become inspectable")
		}
		r.HostPID = cmd.Process.Pid
		r.ShimPID = r.HostPID
		go func() { _ = cmd.Wait() }() // Reap independently; daemon disappearance does not stop the host.
	} else {
		args := []string{"systemd-run", "--user", "--no-block", "--collect", "--service-type=exec", "--unit=" + r.UnitName, "--property=Restart=no", "--property=KillMode=control-group", "--"}
		args = append(args, argv...)
		if _, err = p.cfg.Command(ctx, args); err != nil {
			return r, fail("outcome_unknown", "unit submit may have happened; inspect before recovery")
		}
	}
	if err = WritePrivateJSON(metadataPath(r), r); err != nil {
		return r, fail("outcome_unknown", "submit succeeded but receipt commit failed")
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		inspection, e := p.Inspect(ctx, r)
		if e == nil && !inspection.Gone {
			return inspection.Receipt, nil
		}
		select {
		case <-ctx.Done():
			return r, fail("outcome_unknown", "submit pending at cancellation")
		case <-deadline.C:
			return r, fail("outcome_unknown", "submit pending; inspect before recovery")
		case <-tick.C:
		}
	}
}

// Descriptor validates the exact identity and paths persisted by the host.
func Descriptor(r Receipt) (shim.Launch, error) {
	var spec shim.Launch
	if err := ReadPrivateJSON(r.DescriptorPath, shim.MaxFrame, &spec); err != nil {
		return spec, err
	}
	if spec.Session != r.Session || spec.Instance != r.Instance || spec.Generation != r.Generation || filepath.Join(spec.ControlDir, "control.sock") != r.SocketPath {
		return spec, fail("identity_mismatch", "descriptor differs from placement")
	}
	return spec, nil
}
func health(ctx context.Context, c *shim.Client, session string) (bool, int, shim.Exit, error) {
	var exit shim.Exit
	payload, _ := json.Marshal(map[string]string{"ping": "inspect"})
	if err := c.SendFrame(shim.Frame{Major: 1, Type: "health", RequestID: "inspect-health", Session: session, Epoch: c.Epoch, Body: payload}); err != nil {
		return false, 0, exit, err
	}
	for {
		select {
		case <-ctx.Done():
			return false, 0, exit, ctx.Err()
		case f, ok := <-c.Frames:
			if !ok {
				return false, 0, exit, fail("host_unreachable", "shim disconnected")
			}
			if f.Type == "error" {
				return false, 0, exit, fail("host_unreachable", "shim refused health")
			}
			if f.Type != "result" || f.ReplyTo != "inspect-health" {
				continue
			}
			var b struct {
				Running bool      `json:"running"`
				PID     int       `json:"pid"`
				Exit    shim.Exit `json:"exit"`
			}
			if err := json.Unmarshal(f.Body, &b); err != nil {
				return false, 0, exit, err
			}
			return b.Running, b.PID, b.Exit, nil
		}
	}
}

// Inspect uses authenticated shim health, never a bridge PID. Gone is asserted
// only with a known dead host, or an absent uniquely named systemd unit.
func (p *Provider) Inspect(ctx context.Context, r Receipt) (Inspection, error) {
	spec, err := Descriptor(r)
	if err != nil {
		return Inspection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := shim.Connect(r.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "observer", false)
	if err != nil {
		gone := false
		if r.Backend == Detached && r.HostPID > 0 {
			gone = errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH)
		}
		if r.Backend == SystemdUser {
			if !p.cfg.AllowSystemd || r.UnitName != p.unit(spec) {
				return Inspection{}, fail("identity_mismatch", "foreign or disabled unit")
			}
			b, e := p.cfg.Command(ctx, []string{"systemctl", "--user", "show", r.UnitName, "--property=LoadState", "--value"})
			gone = e == nil && strings.TrimSpace(string(b)) == "not-found"
		}
		if gone {
			return Inspection{Receipt: r, Gone: true}, nil
		}
		return Inspection{}, fail("outcome_unknown", "shim cannot be inspected yet")
	}
	defer func() { _ = c.Close() }()
	if r.Journal != "" && r.Journal != c.Journal {
		return Inspection{}, fail("journal_mismatch", "shim log differs from placement")
	}
	running, pid, exit, err := health(ctx, c, spec.Session)
	if err != nil {
		return Inspection{}, err
	}
	r.Journal = c.Journal
	r.Epoch = c.Epoch
	r.ProviderPID = pid
	if peer, peerErr := peerPID(r.SocketPath); peerErr == nil && peer > 0 {
		r.ShimPID = peer
		r.HostPID = peer
	}
	if r.Backend == SystemdUser {
		b, e := p.cfg.Command(ctx, []string{"systemctl", "--user", "show", r.UnitName, "--property=MainPID", "--value"})
		if e != nil {
			return Inspection{}, fail("outcome_unknown", "unit pid unavailable")
		}
		r.HostPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		r.ShimPID = r.HostPID
	}
	if err = WritePrivateJSON(metadataPath(r), r); err != nil {
		return Inspection{}, err
	}
	return Inspection{Receipt: r, Running: running, Exit: exit}, nil
}

// Reattach inspects an existing placement; it never starts a second child.
func (p *Provider) Reattach(ctx context.Context, r Receipt) (Inspection, error) {
	return p.Inspect(ctx, r)
}

// Stop requests the shim's bounded process-group kill and waits for provider
// exit. It then tears down the host; detached/controller shutdown never calls it.
func (p *Provider) Stop(ctx context.Context, r Receipt) error {
	spec, err := Descriptor(r)
	if err != nil {
		return err
	}
	c, err := shim.Connect(r.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "controller", true)
	if err != nil {
		inspection, e := p.Inspect(ctx, r)
		if e == nil && inspection.Gone {
			return nil
		}
		return err
	}
	defer func() { _ = c.Close() }()
	if r.Journal != "" && r.Journal != c.Journal {
		return fail("journal_mismatch", "refusing stop of another journal")
	}
	if err = c.Send(spec.Session, "control", map[string]string{"action": "kill", "expected_generation": strconv.FormatUint(spec.Generation, 10)}); err != nil {
		return err
	}
	for {
		running, _, _, e := health(ctx, c, spec.Session)
		if e != nil {
			return e
		}
		if !running {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = c.Close()
	if r.Backend == SystemdUser {
		if !p.cfg.AllowSystemd || r.UnitName != p.unit(spec) {
			return fail("identity_mismatch", "foreign unit")
		}
		if _, err = p.cfg.Command(ctx, []string{"systemctl", "--user", "stop", r.UnitName}); err != nil {
			return err
		}
		_, err = p.cfg.Command(ctx, []string{"systemctl", "--user", "reset-failed", r.UnitName})
		return err
	}
	// Authenticate the socket before trusting its host pid. Wait for the exact
	// process to vanish; never kill a stored PID when authentication failed.
	if r.HostPID > 0 {
		if err = syscall.Kill(r.HostPID, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
			return err
		}
	}
	for r.HostPID > 0 && !errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
	return nil
}

// ReadDescriptor is the CLI entry point: no catalog or ambient-home lookup.
func ReadDescriptor(path string) (shim.Launch, error) {
	var spec shim.Launch
	err := ReadPrivateJSON(path, shim.MaxFrame, &spec)
	return spec, err
}
