package main

import (
	"context"
	"os"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/opsstatus"
)

// startOpsStatus runs the Ops status emitter (G3, CW-20261121-0154): session
// lifecycle for task- or workstream-bound sessions goes to the Ops status
// stream. A failure to start never stops the daemon.
func startOpsStatus(ctx context.Context, svc *app.Service, s daemon.OpsStatusSettings, operatorToken string) (func(), error) {
	host, _ := os.Hostname()
	client := &opsstatus.Client{URL: s.URL, TokenFile: s.TokenFile, Actor: s.Actor}
	if client.URL == "" {
		client.URL = "http://127.0.0.1:8994/mcp"
	}
	if client.TokenFile == "" {
		client.TokenFile = operatorToken
	}
	if client.Actor == "" {
		client.Actor = "msg://service/tether/" + host
	}
	emitter := opsstatus.New(svc.Store, svc.Bus, client, opsstatus.Options{Host: host})
	if err := emitter.Start(ctx); err != nil {
		return nil, err
	}
	// The runtime reflex (convention 5.2): remind a session once when its task
	// is done without a work_done post, or blocked without a reason.
	nudger := opsstatus.NewNudger(svc.Store, opsstatus.TorqueFromEnvironment(), svc.SendTurn, 0)
	nudger.Start(ctx)
	return func() { nudger.Close(); emitter.Close() }, nil
}
