// Package channels owns named public topics, independently of mailboxes and
// groups. Channel addresses are derived from names, never consumer identities.
package channels

import (
	"errors"
	"regexp"

	gomsg "github.com/hollis-labs/substrate/mesh/messaging"
)

var ErrInvalid = errors.New("invalid channel request")

var ErrForbidden = errors.New("channel operation forbidden")

var ErrMailboxOperation = errors.New("channel publications are not mailbox items")

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ValidateName accepts one case-sensitive URL path component, at most 64 ASCII
// characters. Names are stable consumer handles; addresses may evolve later.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return ErrInvalid
	}
	return nil
}

// ChannelAddress uses an existing go-messaging kind until the shared library
// supports a channel kind. All construction and recognition lives here.
func ChannelAddress(name string) (gomsg.Address, error) {
	if err := ValidateName(name); err != nil {
		return gomsg.Address{}, err
	}
	return gomsg.Address{Kind: gomsg.KindService, Authority: "local", ID: "channel", SubID: name}, nil
}

// AddressName recognizes only the reserved local channel address namespace.
// A private mailbox's opaque Channel label does not make it a public topic.
func AddressName(a gomsg.Address) (string, bool) {
	if a.Kind != gomsg.KindService || a.Authority != "local" || a.ID != "channel" || ValidateName(a.SubID) != nil {
		return "", false
	}
	return a.SubID, true
}

// NormalizePublication checks and fills the existing channel field. It returns
// false for ordinary mailbox messages, whose channel vocabulary is untouched.
func NormalizePublication(env *gomsg.Envelope) (bool, error) {
	if env.To.Kind != gomsg.KindService || env.To.Authority != "local" || env.To.ID != "channel" {
		return false, nil
	}
	name, ok := AddressName(env.To)
	if !ok || env.From.IsZero() {
		return true, ErrInvalid
	}
	if _, err := gomsg.ParseURN(env.From.URN()); err != nil {
		return true, ErrInvalid
	}
	if env.Channel != "" && string(env.Channel) != name {
		return true, ErrInvalid
	}
	env.Channel = gomsg.Channel(name)
	return true, nil
}
