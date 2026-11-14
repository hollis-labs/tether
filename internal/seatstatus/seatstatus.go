// Package seatstatus is a seat's declared status (CW-20261114-0013, folding the
// finger card CW-20261030-0179 v1): what the seat says it is doing, set with
// tether_status_set, shown beside the live facts the hub observes. Setting it
// is cheap and never gates other work.
package seatstatus

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// State is a declared state.
type State string

const (
	Available       State = "available"
	Working         State = "working"
	WaitingDecision State = "waiting-decision"
	WaitingAgent    State = "waiting-agent"
	Blocked         State = "blocked"
	Paused          State = "paused"
	Offline         State = "offline"
)

// States lists every state, in schema order.
var States = []State{Available, Working, WaitingDecision, WaitingAgent, Blocked, Paused, Offline}

// Limits.
const (
	MaxReason = 200
	MaxRef    = 300
	MaxTTL    = 7 * 24 * time.Hour
)

// Declared is a seat's declared status as stored.
type Declared struct {
	AgentURN  string    `json:"agent_urn"`
	State     State     `json:"state"`
	Reason    string    `json:"reason,omitempty"`
	Ref       string    `json:"ref,omitempty"`
	Owner     string    `json:"owner,omitempty"` // a handle; required for blocked
	Since     time.Time `json:"since"`
	ExpiresAt time.Time `json:"expires_at,omitzero"`
	SetBy     string    `json:"set_by"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Expired reports whether the declaration has lapsed at now.
func (d Declared) Expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && !now.Before(d.ExpiresAt)
}

// SameAs reports whether d and o declare the same thing (state, reason, ref,
// owner). A re-set of the same thing refreshes since/expiry without an event.
func (d Declared) SameAs(o Declared) bool {
	return d.State == o.State && d.Reason == o.Reason && d.Ref == o.Ref && d.Owner == o.Owner
}

// Error is a validation failure: which field, and why.
type Error struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *Error) Error() string { return "status." + e.Field + ": " + e.Reason }

var (
	handleRE = regexp.MustCompile(`^@[a-z][a-z0-9_-]{0,63}$`)
	taskRE   = regexp.MustCompile(`^task:[A-Z][A-Z0-9]*-[0-9]{8}-[0-9]{4}$`)
)

// Input is what a caller asks to declare.
type Input struct {
	State     State
	Reason    string
	Ref       string
	Owner     string
	TTL       time.Duration
	ExpiresAt time.Time
}

// Validate checks the input and returns the expiry it implies at now (zero for
// none). ttl and expires_at are alternatives.
func (in Input) Validate(now time.Time) (time.Time, error) {
	known := false
	for _, s := range States {
		known = known || in.State == s
	}
	if !known {
		names := make([]string, len(States))
		for i, s := range States {
			names[i] = string(s)
		}
		return time.Time{}, &Error{"state", "must be one of " + strings.Join(names, ", ")}
	}
	if n := utf8.RuneCountInString(in.Reason); n > MaxReason || strings.ContainsAny(in.Reason, "\r\n") {
		return time.Time{}, &Error{"reason", fmt.Sprintf("one line of at most %d characters", MaxReason)}
	}
	if in.Ref != "" && !taskRE.MatchString(in.Ref) && !strings.HasPrefix(in.Ref, "https://") {
		return time.Time{}, &Error{"ref", "must be task:<task id> or an https:// URL"}
	}
	if len(in.Ref) > MaxRef {
		return time.Time{}, &Error{"ref", fmt.Sprintf("at most %d bytes", MaxRef)}
	}
	if in.Owner != "" && !handleRE.MatchString(in.Owner) {
		return time.Time{}, &Error{"owner", fmt.Sprintf("%q is not a handle", in.Owner)}
	}
	if in.State == Blocked && in.Owner == "" {
		return time.Time{}, &Error{"owner", "blocked must name who owns the blocker (an @handle)"}
	}
	if in.TTL != 0 && !in.ExpiresAt.IsZero() {
		return time.Time{}, &Error{"ttl", "give ttl or expires_at, not both"}
	}
	expires := in.ExpiresAt
	if in.TTL != 0 {
		if in.TTL < 0 {
			return time.Time{}, &Error{"ttl", "must be positive"}
		}
		expires = now.Add(in.TTL)
	}
	if !expires.IsZero() {
		if !expires.After(now) {
			return time.Time{}, &Error{"expires_at", "is in the past"}
		}
		if expires.Sub(now) > MaxTTL {
			return time.Time{}, &Error{"expires_at", "at most 7 days ahead"}
		}
	}
	return expires.UTC(), nil
}
