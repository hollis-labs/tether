// Package sshenroll implements explicit SSH worker enrollment (ADR 0064).
// It does not register agents, delegate authority, or control provider custody.
package sshenroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
	"github.com/hollis-labs/tether/internal/service"
)

type Problem struct{ Step, Code, Hint string }

func (p *Problem) Error() string {
	return "worker enrollment " + p.Step + ": " + p.Code + "; " + p.Hint
}
func problem(step, code, hint string) error { return &Problem{step, code, hint} }

type Options struct {
	Target, Authority, Version, Archive, Checksums, ReceiptDir string
	Providers, Scopes                                          []string
	RemotePort                                                 int
	Timeout                                                    time.Duration
}

func (o Options) validate() error {
	if !validTarget(o.Target) || o.Authority == "" || environment.ValidateAuthority(o.Authority) != nil || service.ValidateVersion(o.Version) != nil {
		return problem("input", "invalid-target", "Use an SSH host alias or user@host, an authority name, and an exact release version.")
	}
	if o.RemotePort < 1024 || o.RemotePort > 65535 || o.Timeout <= 0 || o.Timeout > 30*time.Minute {
		return problem("input", "invalid-limit", "Use an unprivileged remote port and a positive enrollment timeout up to 30m.")
	}
	scopes, err := identity.NormalizeDeviceScopes(o.Scopes)
	if err != nil {
		return problem("input", "invalid-scope", "Choose explicit independent device scopes, including read for enrollment verification.")
	}
	read := false
	for _, s := range scopes {
		read = read || s == identity.ScopeRead
	}
	if !read {
		return problem("input", "read-required", "Enrollment verifies the device with a protected read; include read explicitly.")
	}
	if len(o.Providers) == 0 {
		return problem("input", "provider-required", "Choose at least one preinstalled provider CLI.")
	}
	for _, p := range o.Providers {
		if p != "claude" && p != "codex" && p != "opencode" && p != "agy" && p != "copilot" && p != "pi" {
			return problem("input", "invalid-provider", "Choose a supported provider CLI name.")
		}
	}
	return nil
}

type WorkerRequest struct {
	OperationID   string   `json:"operation_id"`
	Authority     string   `json:"authority"`
	Version       string   `json:"version"`
	ArchiveSHA256 string   `json:"archive_sha256"`
	RemotePort    int      `json:"remote_port"`
	Scopes        []string `json:"scopes"`
	DeviceID      string   `json:"device_id,omitempty"`
}

type WorkerState struct {
	OperationID   string `json:"operation_id"`
	EnvironmentID string `json:"environment_id"`
	Authority     string `json:"authority"`
	Version       string `json:"version"`
	Phase         string `json:"phase"`
	RemotePort    int    `json:"remote_port"`
	Managed       bool   `json:"managed"`
}

type Tunnel interface {
	BaseURL() string
	Close() error
}
type Remote interface {
	Preflight(context.Context, Options) (Preflight, error)
	Install(context.Context, WorkerRequest, *Artifact) (WorkerState, error)
	Inspect(context.Context, WorkerRequest) (WorkerState, error)
	Grant(context.Context, WorkerRequest) (identity.IssuedGrant, error)
	Forward(context.Context, int) (Tunnel, error)
	Rollback(context.Context, WorkerRequest) error
}

type Manager struct {
	Remote     Remote
	HTTPClient *http.Client
}

// Add never repeats an uncertain pairing exchange. Its receipt is durable
// before every remote mutation; a retry reuses the exact operation and identity.
func (m *Manager) Add(ctx context.Context, o Options) (Receipt, error) {
	if err := o.validate(); err != nil {
		return Receipt{}, err
	}
	if m.Remote == nil {
		return Receipt{}, problem("input", "remote-required", "Configure the SSH transport.")
	}
	o.Scopes = normalizedScopes(o.Scopes)
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	preflight, err := m.Remote.Preflight(ctx, o)
	if err != nil {
		return Receipt{}, err
	}
	if err := preflight.Validate(o.Providers); err != nil {
		return Receipt{}, err
	}
	store, unlock, err := openReceipts(ctx, o.ReceiptDir)
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	r, err := store.load()
	if err != nil && !errors.Is(err, io.EOF) {
		return r, err
	}
	if errors.Is(err, io.EOF) {
		if preflight.ExistingState {
			return r, problem("ownership", "external-state", "Use a clean dedicated worker account; existing state is never adopted.")
		}
		r = Receipt{Target: o.Target, Authority: o.Authority, Version: o.Version, RemotePort: o.RemotePort, Scopes: append([]string(nil), o.Scopes...), OperationID: uuid.NewString(), Phase: "preflight", Preflight: preflight}
	} else if !r.matches(o) {
		return r, problem("receipt", "different-operation", "Retain this receipt; use a separate receipt directory for a different enrollment.")
	}
	if r.Phase == "exchange-uncertain" {
		return r, problem("pair", "outcome-unknown", "Do not replay the exchange. Inspect the worker devices through its local operator and explicitly reconcile the retained receipt.")
	}
	if r.Phase == "rolled-back" {
		return r, problem("receipt", "rolled-back", "Retain the receipt and worker state; a new enrollment requires an explicit separate operation.")
	}
	if r.Phase != "complete" && r.Phase != "paired" {
		artifact, err := VerifyArtifact(ctx, o.Archive, o.Checksums, o.Version, preflight.Arch, store.root)
		if err != nil {
			return r, err
		}
		defer func() { _ = artifact.Close() }()
		if r.ArchiveSHA256 != "" && r.ArchiveSHA256 != artifact.SHA256 {
			return r, problem("artifact", "changed-release", "Retry with the same verified release bytes.")
		}
		r.ArchiveSHA256 = artifact.SHA256
		if err := store.save(r); err != nil {
			return r, err
		}
		r.Phase = "installing"
		if err := store.save(r); err != nil {
			return r, err
		}
		state, err := m.Remote.Install(ctx, r.workerRequest(), artifact)
		if err != nil {
			return r, err
		}
		if !state.matches(r) {
			return r, problem("ownership", "worker-mismatch", "Retain the partial state; the worker must prove this exact managed operation.")
		}
		r.EnvironmentID, r.Phase = state.EnvironmentID, "installed"
		if err := store.save(r); err != nil {
			return r, err
		}
	} else {
		state, err := m.Remote.Inspect(ctx, r.workerRequest())
		if err != nil {
			return r, err
		}
		if !state.matches(r) || state.EnvironmentID != r.EnvironmentID {
			return r, problem("ownership", "worker-mismatch", "Do not reuse credentials for a changed worker identity.")
		}
	}
	tunnel, err := m.Remote.Forward(ctx, r.RemotePort)
	if err != nil {
		return r, err
	}
	defer func() { _ = tunnel.Close() }()
	client, err := enrollmentHTTPClient(m.HTTPClient, tunnel.BaseURL(), r.RemotePort)
	if err != nil {
		return r, err
	}
	defer client.CloseIdleConnections()
	d, err := waitDescriptor(ctx, client, tunnel.BaseURL(), r)
	if err != nil {
		return r, err
	}
	if d.EnvironmentID != r.EnvironmentID {
		return r, problem("descriptor", "identity-mismatch", "The public descriptor must match the SSH-proven worker UUID before pairing.")
	}
	if r.Phase == "complete" {
		if err := verifyCredential(ctx, client, tunnel.BaseURL(), r); err != nil {
			return r, err
		}
		return r, nil
	}
	// A device response that reached disk is reusable without issuing a grant.
	if r.DeviceID == "" {
		grant, err := m.Remote.Grant(ctx, r.workerRequest())
		if err != nil {
			return r, err
		}
		if !validGrant(grant, r.Scopes) || grant.Label != "enrollment-"+r.OperationID {
			return r, problem("pair", "invalid-grant", "The worker must return a current one-time grant with the exact requested scope set.")
		}
		r.GrantID, r.Phase = grant.ID, "exchange-uncertain"
		if err := store.save(r); err != nil {
			return r, err
		}
		device, err := exchange(ctx, client, tunnel.BaseURL(), grant.Code, r.Scopes)
		if err != nil {
			return r, err
		}
		r.DeviceID = device.Principal.ID
		if err := store.save(r); err != nil {
			return r, err
		}
		r.CredentialReference, err = store.saveCredential(device.Token)
		if err != nil {
			return r, err
		}
		r.Phase = "paired"
		if err := store.save(r); err != nil {
			return r, err
		}
	}
	if err := verifyCredential(ctx, client, tunnel.BaseURL(), r); err != nil {
		return r, err
	}
	r.Phase = "complete"
	if err := store.save(r); err != nil {
		return r, err
	}
	return r, nil
}

func (s WorkerState) matches(r Receipt) bool {
	id, err := uuid.Parse(s.EnvironmentID)
	return err == nil && id.Version() == 4 && id.String() == s.EnvironmentID && s.Managed && s.OperationID == r.OperationID && s.Authority == r.Authority && s.Version == r.Version && s.RemotePort == r.RemotePort && s.Phase == "running"
}

func (m *Manager) Rollback(ctx context.Context, directory string) (Receipt, error) {
	store, unlock, err := openReceipts(ctx, directory)
	if err != nil {
		return Receipt{}, err
	}
	defer unlock()
	r, err := store.load()
	if err != nil {
		return r, err
	}
	if ssh, ok := m.Remote.(*SSH); ok && ssh.Target != r.Target {
		return r, problem("rollback", "different-target", "Use the retained receipt SSH target.")
	}
	if r.Phase == "rolled-back" {
		return r, nil
	}
	if m.Remote == nil {
		return r, problem("rollback", "remote-required", "Configure the original SSH target.")
	}
	if err := m.Remote.Rollback(ctx, r.workerRequest()); err != nil {
		return r, err
	}
	r.Phase = "rolled-back"
	return r, store.save(r)
}

func jsonInput(v any) io.Reader { data, _ := json.Marshal(v); return strings.NewReader(string(data)) }
func safeStep(step string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return problem(step, "remote-step-failed", "Inspect the retained partial receipt and the worker locally; raw SSH output is withheld.")
}
func portText(port int) string { return fmt.Sprint(port) }
