package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-sdk/capability"
	"github.com/hollis-labs/libs/mcp/plugin-mcp/plugin-sdk/subprocess"
	"github.com/hollis-labs/tether/internal/workerruntime"
	"github.com/spf13/cobra"
)

func pluginCommand() *cobra.Command {
	return &cobra.Command{Use: "plugin", Short: "Serve the Tether plugin for Outpost", Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error { return subprocess.Serve(&workerPlugin{}) }}
}

type workerPlugin struct {
	mu      sync.Mutex
	runtime *workerruntime.Runtime
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
}

func (p *workerPlugin) Init(_ context.Context, params subprocess.InitParams) (subprocess.InitResult, error) {
	root := params.Config["worker_root"]
	if root == "" {
		root = params.DataDir
	}
	r, err := workerruntime.NewRuntime(root, version)
	if err != nil {
		return subprocess.InitResult{}, err
	}
	p.runtime = r
	return subprocess.InitResult{ID: "tether", Name: "Tether worker", Version: version,
		Protocol: subprocess.ProtocolVersion, CapabilityContract: capability.ContractVersion}, nil
}

func (p *workerPlugin) Load(ctx context.Context) (subprocess.LoadResult, error) {
	p.mu.Lock()
	if p.done != nil {
		p.mu.Unlock()
		return subprocess.LoadResult{}, fmt.Errorf("worker already loaded")
	}
	life, cancel := context.WithCancel(context.Background())
	p.cancel, p.done = cancel, make(chan struct{})
	done := p.done
	p.mu.Unlock()
	ready := make(chan struct{})
	go func() {
		err := p.runtime.Run(life, func() { close(ready) })
		p.mu.Lock()
		p.err = err
		p.mu.Unlock()
		close(done)
	}()
	select {
	case <-ready:
		return subprocess.LoadResult{}, nil
	case <-done:
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.err != nil {
			return subprocess.LoadResult{}, fmt.Errorf("worker stopped during startup: %w", p.err)
		}
		return subprocess.LoadResult{}, fmt.Errorf("worker stopped during startup")
	case <-ctx.Done():
		cancel()
		return subprocess.LoadResult{}, ctx.Err()
	}
}

func (p *workerPlugin) Unload(ctx context.Context) error {
	p.mu.Lock()
	cancel, done := p.cancel, p.done
	p.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *workerPlugin) Health(context.Context) (subprocess.HealthStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.done == nil {
		return subprocess.HealthStatus{Message: "not loaded"}, nil
	}
	select {
	case <-p.done:
		return subprocess.HealthStatus{Message: fmt.Sprintf("worker stopped: %v", p.err)}, nil
	default:
		return subprocess.HealthStatus{OK: true}, nil
	}
}

func (p *workerPlugin) HTTPHandle(ctx context.Context, req subprocess.HTTPRequest) (subprocess.HTTPResponse, error) {
	if req.Method != "GET" || req.Path != "/api/plugins/tether/status" {
		return subprocess.HTTPResponse{Status: 404}, nil
	}
	health, err := p.Health(ctx)
	if err != nil {
		return subprocess.HTTPResponse{}, err
	}
	body, err := json.Marshal(map[string]any{"healthy": health.OK, "version": version,
		"sessions": p.runtime.Executor.Inventory(), "hosts": p.runtime.Executor.ScanHosts()})
	return subprocess.HTTPResponse{Status: 200, Headers: map[string]string{"Content-Type": "application/json"}, Body: body}, err
}
