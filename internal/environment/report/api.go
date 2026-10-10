package report

import (
	"encoding/json"
	"net/http"

	"github.com/hollis-labs/tether/internal/environment"
	"github.com/hollis-labs/tether/internal/identity"
)

type API struct {
	Detectors      *Detectors
	Sampler        *Sampler
	SandboxData    SandboxProtectData
	LaunchHostShim bool
	StateRoot      string
	WorkRoot       string
	Profile        *environment.Profile
}

func (a *API) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	p, ok := identity.FromContext(ctx)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	hasRead := false
	for _, scope := range p.Scopes {
		if scope == "read" || scope == "*" {
			hasRead = true
			break
		}
	}
	if !hasRead {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	rep := Report{
		Providers:   a.Detectors.DetectProviders(ctx, a.SandboxData),
		Hosting:     a.Detectors.DetectHosting(ctx, a.LaunchHostShim),
		Filesystem:  a.Detectors.DetectFilesystem(ctx, a.WorkRoot),
		Resources:   a.Sampler.Current(),
		RoleProfile: FromProfile(a.Profile),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}
