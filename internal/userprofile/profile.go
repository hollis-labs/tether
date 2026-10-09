// Package userprofile holds the temporary local user record until Tether has
// the user model tracked by CW-20261008-0137. It is not an authentication source.
package userprofile

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/hollis-labs/libs/util/apppaths"
	"github.com/hollis-labs/substrate/mesh/messaging"
	"github.com/hollis-labs/tether/internal/config"
)

const OperatorURN = "msg://user/agent-mux/operator"

type Alias struct {
	URN   string `json:"urn"`
	Alias string `json:"alias"`
}

type Messaging struct {
	FromDefault string `json:"from_default,omitempty"`
}

type Profile struct {
	URN       string    `json:"urn"`
	Aliases   []Alias   `json:"aliases"`
	Messaging Messaging `json:"messaging"`
}

func Path() (string, error) {
	layout, err := config.ResolveLayout(paths.WithoutMaterialize())
	if err != nil {
		return "", err
	}
	return filepath.Join(layout.DataDir(), "user-profile.json"), nil
}

func Load(path string) (Profile, error) {
	data, err := os.ReadFile(path) //nolint:gosec // server-owned XDG user record
	if errors.Is(err, os.ErrNotExist) {
		return Profile{URN: OperatorURN, Aliases: []Alias{}}, nil
	}
	if err != nil {
		return Profile{}, err
	}
	var p Profile
	if err := json.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("invalid user profile: %w", err)
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	if p.Aliases == nil {
		p.Aliases = []Alias{}
	}
	return p, nil
}

func canonical(value string) (messaging.Address, error) {
	a, err := messaging.ParseURN(value)
	if err != nil || value != strings.TrimSpace(value) {
		return messaging.Address{}, errors.New("expected a canonical messaging URN")
	}
	return a, nil
}

var aliasPattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9._-]{0,63}$`)

func (p Profile) Validate() error {
	user, err := canonical(p.URN)
	if err != nil || user.Kind != messaging.KindUser {
		return errors.New("invalid local user URN in user profile")
	}
	seen := map[string]bool{}
	seenURN := map[string]bool{}
	for _, alias := range p.Aliases {
		a, err := canonical(alias.URN)
		name := strings.ToLower(alias.Alias)
		if err != nil || a.Kind != messaging.KindUser || !aliasPattern.MatchString(alias.Alias) || seen[name] || seenURN[alias.URN] {
			return errors.New("invalid or duplicate user profile alias")
		}
		seen[name] = true
		seenURN[alias.URN] = true
	}
	if p.Messaging.FromDefault != "" {
		if _, err := canonical(p.Messaging.FromDefault); err != nil {
			return errors.New("invalid saved messaging.from_default in user profile")
		}
	}
	return nil
}

func (p Profile) DefaultSender() string {
	if p.Messaging.FromDefault != "" {
		return p.Messaging.FromDefault
	}
	return p.URN
}

// Resolve accepts canonical addresses or the explicitly configured readable
// aliases. It never guesses a user identity from an address tail.
func (p Profile) Resolve(value string) (messaging.Address, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "msg://") {
		return canonical(value)
	}
	name := strings.TrimPrefix(value, "@")
	for _, alias := range p.Aliases {
		if strings.EqualFold(alias.Alias, name) {
			return canonical(alias.URN)
		}
	}
	return messaging.Address{}, fmt.Errorf("unknown user profile alias %q", value)
}

// Save replaces the local record atomically; an unsuccessful update leaves
// the last saved preference intact. It does not touch stored messages.
func Save(path string, p Profile) error {
	if err := p.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".user-profile-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
