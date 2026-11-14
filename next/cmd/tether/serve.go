package main

import (
	"context"
	"log/slog"
	"net"

	"github.com/spf13/cobra"

	"github.com/hollis-labs/substrate/module"
	"github.com/hollis-labs/substrate/modules/httphost"
)

// tether is what the commands share: the started framework app. ready, when set, learns the bound
// address (tests listen on port 0).
type tether struct {
	app   *module.App
	ready func(net.Addr)
}

func (t *tether) serveCmd(ctx context.Context) *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Run the Tether hub",
		Args:  cobra.NoArgs,
		RunE:  func(*cobra.Command, []string) error { return t.serve(ctx) },
	}
}

// serve runs the HTTP host until ctx ends (SIGINT or SIGTERM), then drains and returns. At S0 the host
// serves /healthz only; S4 adds the client mounts, the unix socket and the remote listener.
func (t *tether) serve(ctx context.Context) error {
	handler, err := httphost.Handler(t.app, httphost.Options{})
	if err != nil {
		return err
	}
	return httphost.Serve(ctx, t.app, handler, httphost.ServeOptions{
		Ready: func(addr net.Addr) {
			slog.Info("tether listening", "addr", addr.String())
			if t.ready != nil {
				t.ready(addr)
			}
		},
	})
}
