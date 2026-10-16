package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/hollis-labs/libs/mesh/core"

	"github.com/hollis-labs/tether/internal/app"
	"github.com/hollis-labs/tether/internal/daemon"
	"github.com/hollis-labs/tether/internal/definitionresolve"
	"github.com/hollis-labs/tether/internal/fabricstore"
	"github.com/hollis-labs/tether/internal/identity"
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
	if err := indexPolicyDefinitions(context.Background(), definitions, policy); err != nil {
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
	hostOptions := policy.HostOptions()
	ports := teamhost.Ports{Enroller: enroller, Sessions: sessions, Messenger: messenger, Channels: teamruntime.Channels{}}
	if policy.Adoption() {
		// Opt-in only: without it no Adopter exists, Adopt is unavailable and
		// the host can never reach the adopted-member path.
		ports.Adopter = &teamruntime.RegistryAdopter{
			Directory: enroller, Sessions: svc.Store, Owners: identity.NewStore(svc.Store.DB()), Notices: svc.Store,
			Launches: hostOptions.AdoptableLaunches, Definitions: definitions, From: teamsvc.LocalOperator,
		}
	}
	host, err := teamhost.New(svc.Store.DB(), storage, ports, hostOptions)
	if err != nil {
		return nil, noop, err
	}
	var adopter teamsvc.Adopter
	if ports.Adopter != nil {
		adopter = teamruntime.HostAdoptions{Host: host}
	}
	ops, err := teamsvc.New(teamsvc.Deps{
		Principals: teamruntime.Principals{Mode: cfg.IdentityModeFor("teams"), Sessions: enroller},
		Ceilings:   teamspolicy.Ceilings{Policy: policy}, Runs: host, Calls: host,
		Definitions: storage, Roster: storage, Ledger: storage, Signals: storage,
		Provisioner: host, Adopter: adopter, Workflows: host, Triggers: host, Sender: host,
		Routing: host, Trust: host, Approvals: host, Clock: host, IDs: host,
		Defaults: teamsvc.ConservativePolicy().Limits,
	})
	if err != nil {
		return nil, noop, err
	}
	complete = true
	svc.BootTeamRecovery = host.RecoverSessions
	stopLoop := teamloop.Start(context.Background(), teamloop.Config{
		Interval: teamLoopInterval(),
		Flush:    host.FlushMessages,
		Reconcile: func(ctx context.Context, limit int) error {
			// Tombstone expired formations before port recovery can retry them.
			expired := host.ReconcileExpiredFormations(ctx, limit)
			ports := (teamruntime.Reconciler{Enroller: enroller, Sessions: sessions, Messenger: messenger}).Reconcile(ctx, limit)
			return errors.Join(expired, ports)
		},
		Logf: log.Printf,
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

// indexPolicyDefinitions indexes each policy entry's authored definition so the
// team host can Load its pin. Nothing else in the daemon indexes definitions;
// without this step every formation fails with a not-found pin. The indexed
// pin must equal the policy pin exactly.
func indexPolicyDefinitions(ctx context.Context, definitions *definitionresolve.DefinitionStore, policy *teamspolicy.Policy) error {
	for _, l := range policy.Launches() {
		if l.Source == "" {
			continue
		}
		verified, err := definitions.Index(ctx, l.Source)
		if err != nil {
			return fmt.Errorf("teams policy launch %q: index %s: %w", l.Launch, l.Source, err)
		}
		if verified.Pin != l.Definition.Ref() {
			return fmt.Errorf("teams policy launch %q: %s is %s/%s %s, policy pins %s/%s %s", l.Launch, l.Source,
				verified.Pin.ID, verified.Pin.Revision, verified.Pin.Digest, l.Definition.ID, l.Definition.Revision, l.Definition.Digest)
		}
	}
	return nil
}
