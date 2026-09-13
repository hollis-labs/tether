package mcpadapter

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/config"
	"github.com/mark3labs/mcp-go/mcp"
)

// RuntimeObservationCapability is an experimental, observation-only wire
// contract. Neither its presence nor a retry count grants permission to exit.
const RuntimeObservationCapability = "hollis-labs.dev/mcp-runtime"

// BuildObservation contains only metadata embedded in this running program.
// Labels and VCS revisions are evidence, not a digest of the running image.
// In particular, never hash os.Executable() to manufacture running identity:
// that pathname may already contain a replacement before our first read.
type BuildObservation struct {
	Version             string `json:"version"`
	Commit              string `json:"commit,omitempty"`
	BuiltAt             string `json:"built_at,omitempty"`
	GoVersion           string `json:"go_version"`
	VCSRevision         string `json:"vcs_revision,omitempty"`
	VCSModified         *bool  `json:"vcs_modified,omitempty"`
	ImageIdentity       string `json:"image_identity"`
	ImageIdentityReason string `json:"image_identity_reason"`
}

type RuntimeObservation struct {
	SchemaVersion int              `json:"schema_version"`
	InstanceID    string           `json:"instance_id"`
	PID           int              `json:"pid"`
	ObservedAt    time.Time        `json:"observed_at"`
	Build         BuildObservation `json:"build"`
}

var processObservation = RuntimeObservation{
	SchemaVersion: 1, InstanceID: uuid.NewString(), PID: os.Getpid(),
	ObservedAt: time.Now().UTC(), Build: embeddedBuildObservation(),
}

func embeddedBuildObservation() BuildObservation {
	b := BuildObservation{Version: "dev", GoVersion: runtime.Version(), ImageIdentity: "unknown", ImageIdentityReason: "running-image-digest-unavailable"}
	if info, ok := debug.ReadBuildInfo(); ok {
		if info.Main.Version != "" && info.Main.Version != "(devel)" {
			b.Version = info.Main.Version
		}
		// Do not serialize Settings: linker flags can contain sensitive values.
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				b.VCSRevision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					modified := setting.Value == "true"
					b.VCSModified = &modified
				}
			}
		}
	}
	return b
}

// SetBuildMetadata wires the CLI's link-time values before serving requests.
// These values survive replacement of the executable on disk. They remain
// descriptive metadata; an unstamped or dirty build must not look proven fresh.
func (a *Adapter) SetBuildMetadata(label, commit, builtAt string) {
	if label != "" && label != "dev" {
		a.runtime.Build.Version = label
	}
	if commit != "" && commit != "unknown" {
		a.runtime.Build.Commit = commit
	}
	if builtAt != "" && builtAt != "unknown" {
		a.runtime.Build.BuiltAt = builtAt
	}
}

// LaunchObservation describes the selector and the owner's pre-spawn lookup,
// never the running leaf image or a promise about a future filesystem lookup.
// PATH is the proxy's PATH (exec.Command resolves before child Env is applied).
// A wrapper can execute another program, so target relation remains unknown.
type LaunchObservation struct {
	Selector       string    `json:"selector,omitempty"`
	SelectorKind   string    `json:"selector_kind"`
	ResolvedPath   string    `json:"resolved_path,omitempty"`
	Resolution     string    `json:"resolution"`
	RelaunchLookup string    `json:"relaunch_lookup"`
	TargetRelation string    `json:"target_relation"`
	PID            int       `json:"pid"`
	ObservedAt     time.Time `json:"observed_at"`
}

type RecoveryObservation struct {
	Mechanism         string `json:"mechanism"`
	AttemptsUsed      int    `json:"attempts_used"`
	AttemptLimit      int    `json:"attempt_limit"`
	AttemptsRemaining int    `json:"attempts_remaining"`
	ExitPermitted     bool   `json:"exit_permitted"`
	Reservation       string `json:"reservation"`
}

type RelaunchObservation struct {
	SchemaVersion int                 `json:"schema_version"`
	Mode          string              `json:"mode"`
	Owner         RuntimeObservation  `json:"owner"`
	Launch        LaunchObservation   `json:"launch"`
	Recovery      RecoveryObservation `json:"recovery"`
}

func observeLaunch(cmd *exec.Cmd, entry config.MCPServerEntry) LaunchObservation {
	o := LaunchObservation{Selector: entry.Command, SelectorKind: "path", Resolution: "unknown", RelaunchLookup: "proxy-PATH", TargetRelation: "unknown", ObservedAt: time.Now().UTC()}
	if filepath.IsAbs(entry.Command) {
		o.SelectorKind, o.RelaunchLookup = "absolute", "selector"
	} else if filepath.Base(entry.Command) != entry.Command {
		o.SelectorKind, o.RelaunchLookup = "relative", "proxy-working-directory"
	}
	if cmd.Err == nil {
		if absolute, err := filepath.Abs(cmd.Path); err == nil {
			if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
				o.ResolvedPath, o.Resolution = resolved, "pre-spawn-path-observation"
			}
		}
	}
	// Paths can themselves include substituted credentials. Suppress both
	// fields if either would disclose one; a redacted path is not a selector.
	for _, secret := range stderrRedactionValues(entry) {
		if secret != "" && (strings.Contains(o.Selector, secret) || strings.Contains(o.ResolvedPath, secret)) {
			o.Selector, o.ResolvedPath, o.Resolution = "", "", "redacted"
			break
		}
	}
	return o
}

func (p *ClientPool) recoveryObservation(s *clientStatus) RecoveryObservation {
	o := RecoveryObservation{Mechanism: "none", Reservation: "none"}
	if s.entry.Transport == "stdio" {
		o.Mechanism = "stdio-exit-bounded-retry"
		o.AttemptLimit = len(p.policy.delays)
		o.AttemptsUsed = s.restarts
		o.AttemptsRemaining = max(0, o.AttemptLimit-o.AttemptsUsed)
	}
	return o
}

func (p *ClientPool) initializeRequest(entry config.MCPServerEntry, client any) mcp.InitializeRequest {
	params := mcp.InitializeParams{ProtocolVersion: mcp.LATEST_PROTOCOL_VERSION, ClientInfo: mcp.Implementation{Name: "agent-mux-proxy", Version: p.runtime.Build.Version}}
	if leaf, ok := client.(*stdioUpstream); ok {
		p.mu.Lock()
		observation := RelaunchObservation{SchemaVersion: 1, Mode: "observation-only", Owner: p.runtime, Launch: leaf.launch, Recovery: p.recoveryObservation(p.statuses[entry.ID])}
		p.mu.Unlock()
		params.Capabilities.Experimental = map[string]any{RuntimeObservationCapability: observation}
	}
	return mcp.InitializeRequest{Params: params}
}
