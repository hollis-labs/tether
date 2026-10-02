package mcpadapter

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/hollis-labs/tether/internal/mcpgateway"
)

// startOrigins serializes only supervisor creation, never a handshake. A slow
// origin cannot block an unrelated view or spawn a second copy of an origin.
func (p *ClientPool) startOrigins(ctx context.Context, ids []string) error {
	p.lazyMu.Lock()
	if p.lazyClosed {
		p.lazyMu.Unlock()
		return fmt.Errorf("MCP pool closed")
	}
	if p.lazyCtx == nil {
		p.lazyCtx, p.cancel = context.WithCancel(ctx)
		p.lazyReady = map[string]chan struct{}{}
	}
	ready := []chan struct{}{}
	for _, id := range ids {
		if ch, ok := p.lazyReady[id]; ok {
			ready = append(ready, ch)
			continue
		}
		for _, entry := range p.entries {
			if entry.ID != id || !entry.IsEnabled() {
				continue
			}
			ch := make(chan struct{})
			p.lazyReady[id] = ch
			ready = append(ready, ch)
			p.mu.Lock()
			p.statuses[id] = &clientStatus{entry: entry, state: "starting"}
			p.mu.Unlock()
			if err := confineUpstreamTransport(entry, p.confineRemote); err != nil {
				p.mu.Lock()
				p.statuses[id].state = "excluded"
				p.statuses[id].err = err
				p.mu.Unlock()
				slog.Warn("daemon MCP upstream excluded", "origin", id, "reason", "cannot be confined locally")
				close(ch)
				continue
			}
			p.workers.Add(1)
			go func() { defer p.workers.Done(); p.supervise(p.lazyCtx, entry, func() { close(ch) }) }()
		}
	}
	p.lazyMu.Unlock()
	for _, ch := range ready {
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	_, collisions := p.registry.NameDiagnostics()
	if len(collisions) > 0 {
		p.mu.Lock()
		for _, c := range collisions {
			for _, o := range c.Owners {
				if s := p.statuses[o.Origin]; s != nil {
					s.degraded = true
					s.err = &mcpgateway.CollisionError{Collisions: []mcpgateway.NameCollision{c}}
				}
			}
		}
		p.mu.Unlock()
		return &mcpgateway.CollisionError{Collisions: collisions}
	}
	return nil
}
