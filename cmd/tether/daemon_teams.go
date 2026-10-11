package main

import (
	"context"
	"log"
	"os"
	"time"

	"github.com/hollis-labs/substrate/mesh"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/teamhost"
	"github.com/hollis-labs/tether/internal/teamloop"
	"github.com/hollis-labs/tether/internal/teamruntime"
	"github.com/hollis-labs/tether/internal/teamspolicy"
	"github.com/hollis-labs/tether/internal/teamstore"
	"github.com/hollis-labs/tether/internal/teamsvc"
)

// buildDaemonTeams borrows the daemon's existing store/runtime. Construction
// performs no migration, enrollment, launch, publication or recovery scheduling.
// The returned closer owns only the read-only authored-content root descriptor.
func buildDaemonTeams(svc *app.Service, cfg daemon.Config) (*teamsvc.Service, func(), error) {
	noop := func() {}
	if !cfg.TeamsEnabled {
		return nil, noop, nil
	}
	// Default deny: an absent policy file leaves formation refused; an invalid
	// one fails startup with the offending entry named.
	policy, err := teamspolicy.Load(svc.CatalogRoot)
	if err != nil {
		return nil, noop, err
	}
	storage, err := teamstore.New(svc.Store.DB(), teamstore.Options{})
	if err != nil {
		return nil, noop, err
	}
	content, err := definitionresolve.NewLocalContent(svc.CatalogRoot)
	if err != nil {
		return nil, noop, err
	}
	closeContent := func() { _ = content.Close() }
	complete := false
	defer func() {
		if !complete {
			closeContent()
		}
	}()
	definitions, err := definitionresolve.NewDefinitionStore(fabricstore.New(svc.Store.DB()), content, definitionresolve.Policy{KnownCapability: teamspolicy.KnownCapability})
	if err != nil {
		return nil, noop, err
	}
	enroller, err := teamruntime.NewLegacyEnroller(svc.Store.DB(), storage, svc.Registry, definitions, launchTargets(policy))
	if err != nil {
		return nil, noop, err
	}
	sessions, err := teamruntime.NewSessions(svc.Store, storage, svc, enroller)
	if err != nil {
		return nil, noop, err
	}
	messenger, err := teamruntime.NewMessenger(svc.Store.DB(), storage, svc)
	if err != nil {
		return nil, noop, err
	}
	host, err := teamhost.New(svc.Store.DB(), storage, teamhost.Ports{Enroller: enroller, Sessions: sessions, Messenger: messenger, Channels: teamruntime.Channels{}}, policy.HostOptions())
	if err != nil {
		return nil, noop, err
	}
	ops, err := teamsvc.New(teamsvc.Deps{
		Principals: teamruntime.Principals{Mode: cfg.IdentityModeFor("teams"), Sessions: enroller},
		Ceilings:   teamspolicy.Ceilings{Policy: policy}, Runs: host, Calls: host,
		Definitions: storage, Roster: storage, Ledger: storage, Signals: storage,
		Provisioner: host, Workflows: host, Triggers: host, Sender: host,
		Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host,
		Defaults: teamsvc.ConservativePolicy().Limits,
	})
	if err != nil {
		return nil, noop, err
	}
	complete = true
	svc.BootTeamRecovery = host.RecoverSessions
	stopLoop := teamloop.Start(context.Background(), teamloop.Config{
		Interval:  teamLoopInterval(),
		Flush:     host.FlushMessages,
		Reconcile: teamruntime.Reconciler{Enroller: enroller, Sessions: sessions, Messenger: messenger}.Reconcile,
		Logf:      log.Printf,
	})
	return ops, func() { stopLoop(); closeContent() }, nil
}

// teamLoopInterval reads TETHER_TEAMS_LOOP_INTERVAL (a Go duration); invalid
// or non-positive values fall back to the 5s default.
func teamLoopInterval() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("TETHER_TEAMS_LOOP_INTERVAL")); err == nil && d > 0 {
		return d
	}
	return teamloop.DefaultInterval
}

func launchTargets(policy *teamspolicy.Policy) map[mesh.DefinitionRef]teamruntime.LaunchTarget {
	out := map[mesh.DefinitionRef]teamruntime.LaunchTarget{}
	for pin, launch := range policy.LaunchTargets() {
		out[pin] = teamruntime.LaunchTarget{LaunchID: launch}
	}
	return out
}
