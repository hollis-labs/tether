//go:build !windows

// Package shimhost places a shim with its own control connection. Detached
// placement stays in the daemon cgroup; systemd-user isolates the host cgroup.
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
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	StateDir        string
	ShimCommand     []string
	HostEnv         []string
	Backend         string
	AllowSystemd    bool
	JournalBytes    int64
	VolatileEnvKeys []string
	StopGrace       time.Duration
	StopTimeout     time.Duration
	LockWait        time.Duration
	// Command is the systemd control seam; nil uses exec.CommandContext. Detached
	// placement always executes the shim itself, not this seam.
	Command func(context.Context, []string) ([]byte, error)
}
type Provider struct {
	cfg    Config
	mu     sync.Mutex
	reaped map[int]chan struct{}
}

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
	Retired        bool   `json:"retired,omitempty"`
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
	if cfg.StopGrace == 0 {
		cfg.StopGrace = 250 * time.Millisecond
	}
	if cfg.StopTimeout == 0 {
		cfg.StopTimeout = 10 * time.Second
	}
	if cfg.LockWait == 0 {
		cfg.LockWait = 2 * time.Second
	}
	if cfg.StopGrace < 0 || cfg.StopTimeout < 0 || cfg.LockWait < 0 {
		return nil, fail("invalid_config", "host durations must be positive")
	}
	return &Provider{cfg: cfg, reaped: make(map[int]chan struct{})}, nil
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
	lock, err := LockWait(ctx, filepath.Join(dir, "placement.lock"), p.cfg.LockWait)
	if err != nil {
		return zero, err
	}
	defer func() { _ = lock.Close() }()
	if err = ClearCommitTemps(dir); err != nil {
		return zero, err
	}
	spec.Secret = ""
	spec.ControlDir = filepath.Join(dir, "c")
	spec.JournalDir = filepath.Join(dir, "j")
	if spec.JournalBytes == 0 {
		spec.JournalBytes = p.cfg.JournalBytes
	}
	if len(filepath.Join(spec.ControlDir, "control.sock")) >= 104 {
		return zero, fail("invalid_request", "control socket path too long")
	}
	fingerprintSpec := spec
	fingerprintSpec.Env = nil
	type envIdentity struct {
		Key      string
		Value    string
		Volatile bool
	}
	env := make([]envIdentity, 0, len(spec.Env))
	volatile := make(map[string]bool, len(p.cfg.VolatileEnvKeys))
	for _, key := range p.cfg.VolatileEnvKeys {
		volatile[key] = true
	}
	for _, entry := range spec.Env {
		key, value, _ := strings.Cut(entry, "=")
		secretName := strings.ToUpper(key)
		isSecret := false
		for _, marker := range []string{"TOKEN", "SECRET", "KEY", "PASSWORD", "CREDENTIAL"} {
			if strings.Contains(secretName, marker) {
				isSecret = true
				break
			}
		}
		field := envIdentity{Key: key, Volatile: volatile[key] || isSecret}
		if !field.Volatile {
			field.Value = value
		}
		env = append(env, field)
	}
	sort.SliceStable(env, func(i, j int) bool { return env[i].Key < env[j].Key })
	raw, err := json.Marshal(struct {
		Launch shim.Launch
		Env    []envIdentity
	}{fingerprintSpec, env}) //nolint:gosec // Capability cleared; declared and secret-looking env values excluded.
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
			inspection, e := p.inspect(ctx, old)
			if e != nil {
				return old, e
			}
			if inspection.Gone {
				return old, fail("outcome_unknown", "previous submit has no live host; native recovery is required")
			}
			if e = WritePrivateJSON(metadataPath(r), inspection.Receipt); e != nil {
				return r, e
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
		reaped := make(chan struct{})
		p.mu.Lock()
		p.reaped[r.HostPID] = reaped
		p.mu.Unlock()
		go func() { _ = cmd.Wait(); close(reaped) }() // Reap a child owned by this process.
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
		inspection, e := p.inspect(ctx, r)
		if e == nil && !inspection.Gone {
			if e = WritePrivateJSON(metadataPath(r), inspection.Receipt); e != nil {
				return r, e
			}
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
func health(ctx context.Context, c *Client, session string) (bool, int, shim.Exit, error) {
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
				var refusal struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(f.Body, &refusal) != nil {
					return false, 0, exit, fail("invalid_frame", "invalid host refusal")
				}
				if f.ReplyTo != "inspect-health" && refusal.Code == "target_offline" {
					continue
				}
				return false, 0, exit, fail(refusal.Code, "shim refused request")
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
func (p *Provider) inspect(ctx context.Context, r Receipt) (Inspection, error) {
	var saved Receipt
	if err := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err == nil && saved.Retired {
		if !samePlacement(saved, r) {
			return Inspection{}, fail("identity_mismatch", "retired placement differs")
		}
		return Inspection{Receipt: saved, Gone: true}, nil
	}
	if saved.Session != "" {
		if !samePlacement(saved, r) {
			return Inspection{}, fail("identity_mismatch", "placement differs")
		}
		if r.Journal == "" {
			r.Journal = saved.Journal
		}
	}
	spec, err := Descriptor(r)
	if err != nil {
		return Inspection{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, r.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "observer", r.Journal, false)
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
	if peer, peerErr := peerPID(c.socket); peerErr == nil && peer > 0 {
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
	return Inspection{Receipt: r, Running: running, Exit: exit}, nil
}

// Reattach inspects an existing placement; it never starts a second child.
func (p *Provider) Reattach(ctx context.Context, r Receipt) (Inspection, error) {
	return p.Inspect(ctx, r)
}

// Stop requests the shim's bounded process-group kill and waits for provider
// exit. It then tears down the host; detached/controller shutdown never calls it.
func (p *Provider) Stop(ctx context.Context, r Receipt) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.StopTimeout)
	defer cancel()
	lock, err := LockWait(ctx, filepath.Join(filepath.Dir(r.DescriptorPath), "placement.lock"), p.cfg.LockWait)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	var saved Receipt
	if e := ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); e == nil {
		if !samePlacement(saved, r) {
			return fail("identity_mismatch", "placement differs")
		}
		if saved.Retired {
			return cleanupRetired(saved)
		}
		if r.Journal == "" {
			r.Journal = saved.Journal
		}
		if r.HostPID == 0 {
			r.HostPID = saved.HostPID
			r.ShimPID = saved.ShimPID
		}
	}
	spec, err := Descriptor(r)
	if err != nil {
		return err
	}
	if r.Backend == SystemdUser && (!p.cfg.AllowSystemd || r.UnitName != p.unit(spec)) {
		return fail("identity_mismatch", "foreign or disabled unit")
	}
	var process *processHandle
	peerGone := false
	c, err := connect(ctx, r.SocketPath, spec.Secret, spec.Session, spec.Instance, strconv.FormatUint(spec.Generation, 10), "controller", r.Journal, true, []int{r.HostPID}, func(socket *net.UnixConn) error {
		var e error
		process, e = authenticatedProcess(&Client{socket: socket}, r.HostPID)
		peerGone = errors.Is(e, syscall.ESRCH)
		return e
	})
	if process != nil {
		defer process.close()
	}
	if err != nil {
		// ESRCH from the peer-pidfd source proves that the connected peer exited.
		// Failed dialing alone never establishes absence.
		if peerGone || p.knownGone(ctx, r) {
			return p.retire(r, saved)
		}
		return err
	}
	defer func() { _ = c.Close() }()
	running, _, _, err := health(ctx, c, spec.Session)
	if err != nil {
		return err
	}
	if running {
		if err = c.Send(spec.Session, "control", map[string]string{"action": "kill", "expected_generation": strconv.FormatUint(spec.Generation, 10)}); err != nil {
			return err
		}
	}
	for running {
		running, _, _, err = health(ctx, c, spec.Session)
		if err != nil {
			return err
		}
		if running {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(20 * time.Millisecond):
			}
		}
	}
	_ = c.Close()
	if err = process.signal(syscall.SIGTERM); err != nil {
		return err
	}
	grace, graceCancel := context.WithTimeout(ctx, p.cfg.StopGrace)
	err = process.wait(grace)
	graceCancel()
	if err != nil {
		if err = process.signal(syscall.SIGKILL); err != nil {
			return err
		}
		if err = process.wait(ctx); err != nil {
			return err
		}
	}
	p.mu.Lock()
	reaped := p.reaped[process.pid]
	p.mu.Unlock()
	if reaped != nil {
		select {
		case <-reaped:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	var serviceErr error
	if r.Backend == SystemdUser {
		_, serviceErr = p.cfg.Command(ctx, []string{"systemctl", "--user", "stop", r.UnitName})
		if serviceErr == nil {
			_, serviceErr = p.cfg.Command(ctx, []string{"systemctl", "--user", "reset-failed", r.UnitName})
		}
		if serviceErr != nil && p.unitGone(ctx, r) {
			serviceErr = nil
		}
	}
	r.Epoch = c.Epoch
	r.Journal = c.Journal
	r.HostPID = process.pid
	r.ShimPID = process.pid
	if err = p.retire(r, saved); err != nil {
		return err
	}
	return serviceErr
}

func (p *Provider) unitGone(ctx context.Context, r Receipt) bool {
	b, err := p.cfg.Command(ctx, []string{"systemctl", "--user", "show", r.UnitName, "--property=LoadState", "--value"})
	return err == nil && strings.TrimSpace(string(b)) == "not-found"
}
func (p *Provider) knownGone(ctx context.Context, r Receipt) bool {
	if r.Backend == SystemdUser {
		return p.unitGone(ctx, r)
	}
	if r.HostPID <= 0 {
		return false
	}
	p.mu.Lock()
	reaped := p.reaped[r.HostPID]
	p.mu.Unlock()
	if reaped != nil {
		select {
		case <-reaped:
			return true
		default:
		}
	}
	return errors.Is(syscall.Kill(r.HostPID, 0), syscall.ESRCH)
}
func (p *Provider) retire(r, saved Receipt) error {
	if saved.Attempted {
		if r.Epoch != "" {
			saved.Epoch = r.Epoch
		}
		if r.Journal != "" {
			saved.Journal = r.Journal
		}
		saved.HostPID = r.HostPID
		saved.ShimPID = r.ShimPID
		r = saved
	}
	r.Retired = true
	if err := WritePrivateJSON(metadataPath(r), r); err != nil {
		return err
	}
	p.mu.Lock()
	delete(p.reaped, r.HostPID)
	p.mu.Unlock()
	return cleanupRetired(r)
}

func samePlacement(a, b Receipt) bool {
	return a.Session == b.Session && a.Instance == b.Instance && a.Generation == b.Generation && a.Backend == b.Backend && a.DescriptorPath == b.DescriptorPath && a.SocketPath == b.SocketPath && a.UnitName == b.UnitName && (a.Journal == "" || b.Journal == "" || a.Journal == b.Journal)
}

// Inspect preserves the durable operation intent and merges only observed facts.
func (p *Provider) Inspect(ctx context.Context, r Receipt) (Inspection, error) {
	observed, err := p.inspect(ctx, r)
	if err != nil {
		return observed, err
	}
	lock, err := LockWait(ctx, filepath.Join(filepath.Dir(r.DescriptorPath), "placement.lock"), p.cfg.LockWait)
	if err != nil {
		return Inspection{}, err
	}
	defer func() { _ = lock.Close() }()
	var saved Receipt
	if err = ReadPrivateJSON(metadataPath(r), shim.MaxFrame, &saved); err != nil {
		return observed, err
	}
	if !samePlacement(saved, observed.Receipt) {
		return Inspection{}, fail("identity_mismatch", "observed placement differs")
	}
	if saved.Retired {
		return Inspection{Receipt: saved, Gone: true}, nil
	}
	saved.Journal = observed.Receipt.Journal
	oldEpoch, _ := strconv.ParseUint(saved.Epoch, 10, 64)
	newEpoch, _ := strconv.ParseUint(observed.Receipt.Epoch, 10, 64)
	if newEpoch >= oldEpoch {
		saved.Epoch = observed.Receipt.Epoch
	}
	saved.HostPID = observed.Receipt.HostPID
	saved.ShimPID = observed.Receipt.ShimPID
	saved.ProviderPID = observed.Receipt.ProviderPID
	if err = WritePrivateJSON(metadataPath(r), saved); err != nil {
		return Inspection{}, err
	}
	observed.Receipt = saved
	return observed, nil
}

// ReadDescriptor is the CLI entry point: no catalog or ambient-home lookup.
func ReadDescriptor(path string) (shim.Launch, error) {
	var spec shim.Launch
	err := ReadPrivateJSON(path, shim.MaxFrame, &spec)
	return spec, err
}

func cleanupRetired(r Receipt) error {
	if f, err := privateFile(r.DescriptorPath); err == nil {
		_ = f.Close()
		if err = os.Remove(r.DescriptorPath); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return ClearCommitTemps(filepath.Dir(r.DescriptorPath))
}
