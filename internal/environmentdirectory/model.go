// Package environmentdirectory owns the hub's durable environment bindings.
// Directory presence and management mode never confer worker authorization.
package environmentdirectory

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	messaging "github.com/hollis-labs/substrate/mesh/messaging"
	tether "github.com/hollis-labs/substrate/mesh/tetherclient"
)

var (
	ErrInvalid  = errors.New("environment directory: invalid request")
	ErrConflict = errors.New("environment directory: immutable binding conflict")
	ErrNotFound = errors.New("environment directory: not found")
	ErrRetired  = errors.New("environment directory: retired")
)

type Mode string

const (
	Independent Mode = "INDEPENDENT"
	HubManaged  Mode = "HUB-MANAGED"
)

// ResolveMode applies DEC102 at deployment/launch: launch > agent > instance.
// Missing declarations default independent; invalid declarations are refused,
// including an invalid lower-precedence declaration. This is not admission.
func ResolveMode(instance, agent, launch Mode) (Mode, error) {
	result := Independent
	for _, m := range []Mode{instance, agent, launch} {
		if m == "" {
			continue
		}
		if m != Independent && m != HubManaged {
			return "", fmt.Errorf("%w: unknown management mode", ErrInvalid)
		}
		result = m
	}
	return result, nil
}

type Home struct {
	URN            string `json:"urn"`
	Authority      string `json:"authority"`
	ManagementMode Mode   `json:"managementMode,omitempty"`
}

type Registration struct {
	tether.EnvironmentTarget
	Label          string `json:"label"`
	DeviceID       string `json:"deviceId"`
	Ownership      string `json:"ownership"`
	ManagementMode Mode   `json:"managementMode,omitempty"`
	Homes          []Home `json:"homes,omitempty"`
}

type Record struct {
	Registration
	Capabilities      map[string]map[string]any `json:"capabilities"`
	Protocol          int                       `json:"protocol"`
	ServerVersion     string                    `json:"serverVersion"`
	State             string                    `json:"state"`
	LastSeen          *time.Time                `json:"lastSeen,omitempty"`
	RevocationPending bool                      `json:"revocationPending"`
}

var authorityPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Normalize validates declarations, without opening a file or dialing a route.
func Normalize(in Registration) (Registration, error) {
	id, err := uuid.Parse(in.EnvironmentID)
	if err != nil || id.Version() != 4 || id.Variant() != uuid.RFC4122 || id.String() != in.EnvironmentID || !authorityPattern.MatchString(in.Authority) {
		return Registration{}, fmt.Errorf("%w: expected UUIDv4 and canonical authority", ErrInvalid)
	}
	if len(in.Label) > 256 || strings.ContainsAny(in.Label, "\x00\r\n") || len(in.Homes) > 4096 {
		return Registration{}, fmt.Errorf("%w: invalid label or home list", ErrInvalid)
	}
	if in.Ownership != "managed" && in.Ownership != "external" {
		return Registration{}, fmt.Errorf("%w: explicit deployment ownership required", ErrInvalid)
	}
	in.ManagementMode, err = ResolveMode(in.ManagementMode, "", "")
	if err != nil {
		return Registration{}, err
	}
	if len(in.Routes) == 0 || len(in.Routes) > 32 {
		return Registration{}, fmt.Errorf("%w: explicit durable routes required", ErrInvalid)
	}
	in.Routes = append([]tether.EnvironmentRoute(nil), in.Routes...)
	seen := map[string]bool{}
	for i, r := range in.Routes {
		u, e := url.Parse(r.BaseURL)
		if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || strings.ContainsAny(r.BaseURL, "\x00\r\n") {
			return Registration{}, fmt.Errorf("%w: invalid route", ErrInvalid)
		}
		in.Routes[i].BaseURL = strings.TrimRight(r.BaseURL, "/")
		if seen[in.Routes[i].BaseURL] {
			return Registration{}, fmt.Errorf("%w: duplicate route", ErrInvalid)
		}
		seen[in.Routes[i].BaseURL] = true
	}
	if _, err = CredentialPath(in.CredentialReference); err != nil {
		return Registration{}, err
	}
	if in.DeviceID == "" || len(in.DeviceID) > 512 || strings.ContainsAny(in.DeviceID, "\x00\r\n") {
		return Registration{}, fmt.Errorf("%w: exact paired device ID required", ErrInvalid)
	}
	in.Homes = append([]Home(nil), in.Homes...)
	seen = map[string]bool{}
	for i, h := range in.Homes {
		addr, e := messaging.ParseURN(h.URN)
		if e != nil || addr.Kind != "agent" || addr.Authority != in.Authority || addr.URN() != h.URN || addr.SubID != "" || strings.ContainsAny(h.URN, "\x00\r\n\t ") || seen[h.URN] {
			return Registration{}, fmt.Errorf("%w: canonical unique agent home required", ErrInvalid)
		}
		seen[h.URN] = true
		if h.Authority == "" {
			in.Homes[i].Authority = "environment"
		} else if h.Authority != "environment" && h.Authority != "hub" {
			return Registration{}, fmt.Errorf("%w: invalid home authority metadata", ErrInvalid)
		}
		if _, err = ResolveMode(in.ManagementMode, h.ManagementMode, ""); err != nil {
			return Registration{}, err
		}
	}
	sort.Slice(in.Homes, func(i, j int) bool { return in.Homes[i].URN < in.Homes[j].URN })
	return in, nil
}

// CredentialPath accepts only an explicit file reference; validation reads no
// credential bytes. The resolver applies current-user ownership and mode checks.
func CredentialPath(ref string) (string, error) {
	u, err := url.Parse(ref)
	if err != nil || u.Scheme != "file" || u.Host != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || !strings.HasPrefix(ref, "file:///") || !filepath.IsAbs(u.Path) || u.Path == "/" || strings.ContainsAny(u.Path, "\x00\r\n") {
		return "", fmt.Errorf("%w: explicit private file credential reference required", ErrInvalid)
	}
	return u.Path, nil
}

// Public removes the private credential locator, while preserving all identity
// and observed state. No projection ever contains a token value.
func (r Record) Public() Record { r.CredentialReference = ""; return r }

// SameBinding excludes display/observation fields; immutable deployment inputs
// cannot turn a repeated registration into a runtime authority switch.
func SameBinding(a, b Registration) bool {
	a.Label, b.Label = "", ""
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
