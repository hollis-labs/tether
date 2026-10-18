package workerruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hollis-labs/tether/internal/outpostmail"
	"github.com/hollis-labs/tether/internal/pulse"
	"github.com/hollis-labs/tether/internal/workerwire"
)

// Runtime composes the existing worker channel, local mail, and session executor.
// Stopping it leaves shim-host processes available for recovery by its successor.
type Runtime struct {
	Worker   *Worker
	Executor *Executor
	Mail     *outpostmail.Server
}

func NewRuntime(root, version string) (*Runtime, error) {
	cfg, err := Load(root)
	if err != nil {
		return nil, err
	}
	w := &Worker{Root: root, Config: cfg, Version: version, Activity: pulse.New().Scan}
	if cfg.Path != "" {
		if err := os.Setenv("PATH", cfg.Path); err != nil {
			return nil, err
		}
	}
	binary, err := os.Executable()
	if err != nil {
		return nil, err
	}
	executor := NewExecutor(root, binary, w.Send)
	executor.Config = cfg
	executor.Tokens = &TokenMinter{Hub: cfg.Hub, DeviceTokenFile: filepath.Join(root, "device.token")}
	executor.HubMCPURL = strings.TrimRight(cfg.Hub, "/") + "/mcp"
	executor.AllowSSHFallback = cfg.MCPSSHFallback || os.Getenv("TETHER_WORKER_MCP_SSH_FALLBACK") == "1"
	if err := executor.Restore(); err != nil {
		return nil, err
	}
	w.Inventory = executor.Inventory
	w.Hosts = executor.ScanHosts
	mail := &outpostmail.Server{Socket: cfg.MailSocket, State: filepath.Join(root, "mail.json"), AllowedUIDs: cfg.MailUIDs, GID: -1}
	if mail.Socket == "" {
		mail.Socket = filepath.Join(root, "run", "mail.sock")
	}
	if cfg.MailGID != nil {
		mail.GID = *cfg.MailGID
	}
	mail.Bind = func(ctx context.Context, b workerwire.Binding) (workerwire.Binding, error) {
		wait, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		r, err := w.Request(wait, workerwire.Frame{T: "bind", CmdID: uuid.NewString(), Binding: &b})
		if err != nil {
			if errors.Is(err, ErrHubDisconnected) || errors.Is(err, context.DeadlineExceeded) {
				return b, outpostmail.ErrHubOffline
			}
			return b, err
		}
		if r.Binding == nil {
			return b, fmt.Errorf("missing bind acknowledgement")
		}
		return *r.Binding, nil
	}
	mail.Unbind = func(ctx context.Context, b workerwire.Binding) error {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := w.Request(wait, workerwire.Frame{T: "unbind", CmdID: uuid.NewString(), Binding: &b})
		return err
	}
	w.Bindings = mail.Inventory
	w.Command = func(ctx context.Context, f workerwire.Frame) error {
		if f.T == "welcome" {
			mail.Welcome(f.Bindings)
		}
		if f.T == "deliver" {
			wait, cancel := context.WithTimeout(ctx, 12*time.Second)
			defer cancel()
			err := mail.Deliver(wait, f)
			r := workerwire.Frame{T: "deliver_ack", CmdID: f.CmdID}
			if err != nil {
				r.Error = err.Error()
			}
			return w.Send(r)
		}
		return executor.Command(ctx, f)
	}
	return &Runtime{Worker: w, Executor: executor, Mail: mail}, nil
}

// Run drains the worker and mail loops before returning. Ready is called only
// after the local registration socket has been bound with its final permissions.
func (r *Runtime) Run(ctx context.Context, ready func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	r.Mail.Ready = ready
	done := make(chan error, 2)
	go func() { done <- r.Mail.Serve(ctx) }()
	go r.Executor.ReapLoop(ctx, HostReapInterval, OrphanGrace)
	go func() { done <- r.Worker.Run(ctx) }()
	err := <-done
	cancel()
	return errors.Join(err, <-done)
}
