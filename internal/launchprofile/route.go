package launchprofile

import (
	"fmt"
	"regexp"
	"slices"
)

// Route opts a launch into per-turn publishing to a named channel (ADR 0049).
// Nil means no routing. Replies always return to the sender.
type Route struct {
	Channel string   `yaml:"channel" json:"channel"`
	Kinds   []string `yaml:"kinds" json:"kinds"`
}

// InvalidRouteError identifies an invalid route independently of its message.
type InvalidRouteError struct{ Field, Value string }

func (e *InvalidRouteError) Error() string {
	return fmt.Sprintf("invalid route %s %q", e.Field, e.Value)
}

// Temporary channel-name syntax; shared channel validation replaces this seam.
var channelName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// ResolveRoute validates and copies the route, filling the default kinds.
// An explicit empty list is preserved: it publishes no kinds.
func ResolveRoute(in *Route) (*Route, error) {
	if in == nil {
		return nil, nil
	}
	if !channelName.MatchString(in.Channel) {
		return nil, &InvalidRouteError{Field: "channel", Value: in.Channel}
	}
	out := &Route{Channel: in.Channel, Kinds: slices.Clone(in.Kinds)}
	if out.Kinds == nil {
		out.Kinds = []string{"final", "question", "approval", "failure"}
	}
	for _, kind := range out.Kinds {
		switch kind {
		case "final", "question", "approval", "failure":
		default:
			return nil, &InvalidRouteError{Field: "kinds", Value: kind}
		}
	}
	return out, nil
}
