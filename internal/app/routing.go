package app

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/store"
)

// These optional composition seams report installed paths, not configured
// intent. CW-0064 supplies RoutingWiring from its running router and publisher;
// reply and interrupt stay unavailable until their own service paths exist.
type routingWiring interface{ RoutingWiring() ([]string, bool) }
type routingRuntimeKinds interface{ RoutingRuntimeKinds(string) []string }
type routingReplyWiring interface{ RoutingReplyWired() bool }
type routingInterruptWiring interface{ RoutingInterruptWired(string) bool }

func (s *Service) RoutingCapabilities(ctx context.Context, sessionID string) (RoutingCapabilitiesResponse, error) {
	return s.routingCapabilities(ctx, sessionID, s)
}

func (s *Service) routingCapabilities(ctx context.Context, sessionID string, source any) (RoutingCapabilitiesResponse, error) {
	if err := ctx.Err(); err != nil {
		return RoutingCapabilitiesResponse{}, err
	}
	out := RoutingCapabilitiesResponse{Delivery: "next-turn", KindsAvailable: []string{}, Runtimes: map[string]RuntimeRoutingCapabilities{}, SessionID: sessionID}
	providers := map[string]config.Provider{}
	if s.Catalog != nil {
		for id, p := range s.Catalog.Providers {
			providers[id] = p
		}
	}
	if sessionID != "" {
		if s.Store == nil {
			return out, ErrRoutingSessionNotFound
		}
		row, err := s.Store.GetSession(sessionID)
		if errors.Is(err, store.ErrSessionNotFound) {
			return out, ErrRoutingSessionNotFound
		}
		if err != nil {
			return out, err
		}
		p, ok := providers[row.ProviderID]
		plan, err := s.Store.GetLaunchPlan(sessionID)
		if err == nil && plan.ProviderBrand != "" {
			p = config.Provider{ID: row.ProviderID, Provider: plan.ProviderBrand, RuntimeKind: plan.RuntimeKind}
			ok = true
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return out, err
		}
		providers = map[string]config.Provider{}
		if ok {
			providers[row.ProviderID] = p
		}
	}
	var published []string
	var installed bool
	if wiring, ok := source.(routingWiring); ok {
		published, installed = wiring.RoutingWiring()
	}
	reply := false
	if wiring, ok := source.(routingReplyWiring); ok {
		reply = wiring.RoutingReplyWired()
	}
	// Stable ordering also makes multiple catalog modes for one registry id
	// deterministic. The gateway advertises only their common guarantees.
	ids := make([]string, 0, len(providers))
	for id := range providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		p := providers[id]
		runtimeID, confidence, cancelTurn, err := resolvedRuntimeRouting(p)
		if err != nil {
			continue
		}
		capability := RuntimeRoutingCapabilities{KindsAvailable: []string{}, FinalTextConfidence: "unknown"}
		// Final/failure have a provider-neutral turn-output feed. Questions and
		// approvals require runtime-specific installed detectors (CW-0073).
		runtimeKinds := []string{"final", "failure"}
		if wiring, ok := source.(routingRuntimeKinds); ok {
			runtimeKinds = wiring.RoutingRuntimeKinds(runtimeID)
		}
		if len(published) > 0 {
			capability.FinalTextConfidence = confidence
			if confidence != "none" && confidence != "unknown" {
				capability.KindsAvailable = commonRoutingKinds(published, runtimeKinds)
			}
		}
		capability.RouteSupported = installed && len(capability.KindsAvailable) > 0
		capability.ReplyToSender = reply && len(published) > 0
		if wiring, ok := source.(routingInterruptWiring); ok {
			capability.Interrupt = cancelTurn && wiring.RoutingInterruptWired(id)
		}
		if previous, ok := out.Runtimes[runtimeID]; ok {
			capability.RouteSupported = capability.RouteSupported && previous.RouteSupported
			capability.ReplyToSender = capability.ReplyToSender && previous.ReplyToSender
			capability.Interrupt = capability.Interrupt && previous.Interrupt
			capability.KindsAvailable = commonRoutingKinds(previous.KindsAvailable, capability.KindsAvailable)
			if capability.FinalTextConfidence != previous.FinalTextConfidence {
				capability.FinalTextConfidence = "unknown"
			}
		}
		out.Runtimes[runtimeID] = capability
	}
	for _, capability := range out.Runtimes {
		out.RouteSupported = out.RouteSupported || capability.RouteSupported
		out.ReplyToSender = out.ReplyToSender || capability.ReplyToSender
		out.Interrupt = out.Interrupt || capability.Interrupt
		for _, kind := range capability.KindsAvailable {
			if !containsRoutingKind(out.KindsAvailable, kind) {
				out.KindsAvailable = append(out.KindsAvailable, kind)
			}
		}
	}
	sort.Strings(out.KindsAvailable)
	return out, nil
}

func commonRoutingKinds(a, b []string) []string {
	out := []string{}
	for _, kind := range a {
		if containsRoutingKind(b, kind) {
			out = append(out, kind)
		}
	}
	return out
}

func containsRoutingKind(kinds []string, kind string) bool {
	for _, value := range kinds {
		if value == kind {
			return true
		}
	}
	return false
}
