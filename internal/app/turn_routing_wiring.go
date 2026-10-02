package app

import (
	"context"
	"sort"

	"github.com/hollis-labs/go-agent-wrapper/turnoutput"
	gopevents "github.com/hollis-labs/go-providers/provider/events"
	"github.com/hollis-labs/tether/internal/app/turnrouting"
	"github.com/hollis-labs/tether/internal/messaging/channels"
)

// Registrations describe the installed source handlers. Unknown producers retain
// the generic final/failure feed; advanced signals require an installed handler.
type turnFeedRegistration struct {
	questionTools []string
	approval      func(*sessionTurnOutput, gopevents.PermissionDenied)
}

func (s *Service) installTurnFeeds() {
	s.turnFeeds = make(map[string]turnFeedRegistration)
	for _, p := range s.Catalog.Providers {
		runtimeID, _, _, err := resolvedRuntimeRouting(p)
		if err != nil {
			continue
		}
		registration := turnFeedRegistration{}
		switch runtimeID {
		case "claude", "codex":
			registration.questionTools = append([]string(nil), turnoutput.DefaultQuestionTools...)
			registration.approval = func(o *sessionTurnOutput, e gopevents.PermissionDenied) { o.observeProvider(e) }
		case "antigravity":
			registration.approval = func(o *sessionTurnOutput, e gopevents.PermissionDenied) { o.observeProvider(e) }
		}
		s.turnFeeds[runtimeID] = registration
	}
}

func (r turnFeedRegistration) observe(o *sessionTurnOutput, ev gopevents.Event) {
	if denied, ok := ev.(gopevents.PermissionDenied); ok {
		if r.approval != nil {
			r.approval(o, denied)
		}
		return
	}
	o.observeProvider(ev)
}

func (s *Service) startTurnRouter() error {
	if s.Channels == nil {
		s.Channels = channels.New(s.Store, nil)
	}
	router := turnrouting.New(s.Store, s.Bus, s.Channels)
	if err := router.Start(context.Background()); err != nil {
		return err
	}
	s.turnRouter = router
	return nil
}

// RoutingWiring reports the union of installed producer handlers and the live
// worker. A catalog flag alone never makes routing available.
func (s *Service) RoutingWiring() ([]string, bool) {
	if s.Bus == nil {
		return nil, false
	}
	seen := make(map[string]bool)
	for runtimeID := range s.turnFeeds {
		for _, kind := range s.RoutingRuntimeKinds(runtimeID) {
			seen[kind] = true
		}
	}
	kinds := make([]string, 0, len(seen))
	for kind := range seen {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds, s.turnRouter.Running()
}

func (s *Service) RoutingRuntimeKinds(runtimeID string) []string {
	registration, ok := s.turnFeeds[runtimeID]
	if !ok || s.Bus == nil {
		return nil
	}
	kinds := []string{string(turnoutput.KindFinal), string(turnoutput.KindFailure)}
	if len(registration.questionTools) > 0 {
		kinds = append(kinds, string(turnoutput.KindQuestion))
	}
	if registration.approval != nil {
		kinds = append(kinds, string(turnoutput.KindApproval))
	}
	return kinds
}

func (s *Service) questionTools(runtimeID string) []string {
	registration, ok := s.turnFeeds[runtimeID]
	if !ok {
		return []string{}
	}
	// Empty explicitly disables question tools on a registered source without a
	// detector. The reducer interprets nil as its defaults.
	tools := append([]string{}, registration.questionTools...)
	return tools
}
