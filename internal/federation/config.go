package federation

import (
	"fmt"
	"net/url"
	"strings"
)

// Config is the authority-routing federation block. It is embedded in
// config.Global as the `federation:` YAML key.
//
// The zero value (Enabled=false) is a standalone install: no peers, no
// routing, behavior identical to pre-federation Tether. Federation is
// opt-in — an operator enables it deliberately.
type Config struct {
	// Enabled turns federation on. When false every other field is
	// ignored and the daemon runs exactly as it did before federation
	// existed.
	Enabled bool `yaml:"enabled" json:"enabled"`

	// LocalAuthority is the URN authority segment this daemon owns — its
	// federation "domain". A message whose recipient authority equals
	// this value (or any authority without a registered peer) is served
	// by the local store. Required when Enabled.
	LocalAuthority string `yaml:"local_authority" json:"local_authority"`

	// Strict makes the router reject a message addressed to an authority
	// that is neither LocalAuthority nor a registered peer, instead of
	// falling through to the local store. Off by default — the lenient
	// fall-through keeps a misconfigured peer from hard-failing local
	// traffic. Turn it on when a misaddressed envelope should be a loud
	// error rather than a silent local delivery.
	Strict bool `yaml:"strict" json:"strict"`

	// Peers lists the foreign authorities this daemon can route to.
	Peers []Peer `yaml:"peers" json:"peers,omitempty"`
}

// Peer binds a foreign URN authority to the daemon that owns it.
type Peer struct {
	// Authority is the foreign URN authority segment (e.g. "torque").
	Authority string `yaml:"authority" json:"authority"`

	// BaseURL is the root of that daemon's HTTP messaging surface, e.g.
	// "http://10.0.0.4:7777". The go-messaging /messages/* routes are
	// resolved against it.
	BaseURL string `yaml:"base_url" json:"base_url"`
}

// Validate reports a configuration error in the federation block. A
// disabled block always validates — the zero value is a legal standalone
// install. An enabled block requires a well-formed local authority and
// internally consistent peers.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if err := validAuthority(c.LocalAuthority); err != nil {
		return fmt.Errorf("federation: local_authority %w", err)
	}
	seen := make(map[string]struct{}, len(c.Peers))
	for i, p := range c.Peers {
		if err := validAuthority(p.Authority); err != nil {
			return fmt.Errorf("federation: peers[%d].authority %w", i, err)
		}
		if p.Authority == c.LocalAuthority {
			return fmt.Errorf("federation: peers[%d] authority %q duplicates local_authority", i, p.Authority)
		}
		if _, dup := seen[p.Authority]; dup {
			return fmt.Errorf("federation: peers[%d] authority %q is registered twice", i, p.Authority)
		}
		seen[p.Authority] = struct{}{}
		if err := validBaseURL(p.BaseURL); err != nil {
			return fmt.Errorf("federation: peers[%d] (%s) base_url %w", i, p.Authority, err)
		}
	}
	return nil
}

// validAuthority enforces the URN authority grammar: one non-empty path
// segment with no whitespace and no "/" (which would split the URN).
func validAuthority(a string) error {
	if a == "" {
		return fmt.Errorf("is required")
	}
	if strings.ContainsAny(a, "/ \t\r\n") {
		return fmt.Errorf("%q must be a single URN segment (no slash or whitespace)", a)
	}
	return nil
}

func validBaseURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%q is not a valid URL: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("%q must be an http(s) URL", raw)
	}
	if u.Host == "" {
		return fmt.Errorf("%q must include a host", raw)
	}
	return nil
}
