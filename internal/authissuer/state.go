package authissuer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	auth "github.com/hollis-labs/libs/security/auth"
	"github.com/hollis-labs/tether/internal/identity"
)

// Deny-list kinds, matching the wire document's three arrays.
const (
	DenyJTI = "jti"
	DenySub = "sub"
	DenySID = "sid"
)

type denyEntry struct {
	Kind  string    `json:"kind"`
	Value string    `json:"value"`
	Until time.Time `json:"until,omitzero"` // zero = until removed
}

// SessionGrant is the launch policy for one agent session: what the machine
// outpost (a paired device) or a service launcher may ask the issuer to mint for it. The hub
// registers it when it sends the launch, so an outpost's token requests are
// bounded by what the hub authorized for that session and that device, not by
// the device's own Tether scopes.
type SessionGrant struct {
	Launcher string              `json:"device"` // launching device or service id; json key kept for stored state
	Scopes   map[string][]string `json:"scopes"` // audience -> scopes
	Expires  time.Time           `json:"expires"`
}

// mintedEntry remembers an unexpired token minted from a parent credential
// that can be rotated (the operator or a recorded direct workload), so that rotating it can deny the tokens
// it already issued. It holds ids only, never a token.
type mintedEntry struct {
	JTI string    `json:"jti"`
	Par string    `json:"par"`
	Exp time.Time `json:"exp"`
}

type stateDoc struct {
	Workloads map[string]WorkloadMint `json:"workloads,omitempty"`
	Deny      []denyEntry             `json:"deny"`
	Grants    map[string]SessionGrant `json:"grants"`
	Minted    []mintedEntry           `json:"minted,omitempty"`
	// Services is the metadata of issued service tokens, keyed by jti. Never a token.
	Services map[string]ServiceRecord `json:"services,omitempty"`
}

// State is the issuer's small persisted state: the deny-list and the session
// grants. It lives in one 0600 JSON file so a hub restart does not forget a
// revocation or strand a running session's refresh.
type State struct {
	path string
	now  func() time.Time
	// List mirrors the deny-list for in-process verification.
	List *auth.DenyList

	mu   sync.Mutex
	doc  stateDoc
	etag string
	body []byte
}

// OpenState loads path (created lazily on first write).
func OpenState(path string) (*State, error) {
	s := &State{path: path, now: time.Now, List: auth.NewDenyList(), doc: stateDoc{Grants: map[string]SessionGrant{}}}
	b, err := os.ReadFile(path) //nolint:gosec // G304: the issuer's own state file
	switch {
	case err == nil:
		if err := json.Unmarshal(b, &s.doc); err != nil {
			return nil, err
		}
		if s.doc.Grants == nil {
			s.doc.Grants = map[string]SessionGrant{}
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.rebuildLocked()
	return s, nil
}

func (s *State) pruneLocked() bool {
	now := s.now()
	changed := false
	kept := s.doc.Deny[:0]
	for _, e := range s.doc.Deny {
		if !e.Until.IsZero() && !now.Before(e.Until) {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	s.doc.Deny = kept
	for sid, g := range s.doc.Grants {
		if !now.Before(g.Expires) {
			delete(s.doc.Grants, sid)
			changed = true
		}
	}
	for id, m := range s.doc.Workloads {
		if now.Unix() >= m.Claims.Exp {
			delete(s.doc.Workloads, id)
			changed = true
		}
	}
	keptMinted := s.doc.Minted[:0]
	for _, m := range s.doc.Minted {
		if !now.Before(m.Exp) {
			changed = true
			continue
		}
		keptMinted = append(keptMinted, m)
	}
	s.doc.Minted = keptMinted
	for jti, r := range s.doc.Services {
		if !now.Before(r.ExpiresAt) {
			delete(s.doc.Services, jti)
			changed = true
		}
	}
	return changed
}

func (s *State) rebuildLocked() {
	d := auth.DenyListDoc{JTI: []string{}, Sub: []string{}, SID: []string{}}
	for _, e := range s.doc.Deny {
		switch e.Kind {
		case DenyJTI:
			d.JTI = append(d.JTI, e.Value)
		case DenySub:
			d.Sub = append(d.Sub, e.Value)
		case DenySID:
			d.SID = append(d.SID, e.Value)
		}
	}
	sort.Strings(d.JTI)
	sort.Strings(d.Sub)
	sort.Strings(d.SID)
	s.body, _ = json.Marshal(d)
	sum := sha256.Sum256(s.body)
	s.etag = `"` + hex.EncodeToString(sum[:12]) + `"`
	s.List.Replace(d)
}

func (s *State) saveLocked() error {
	b, err := json.Marshal(s.doc)
	if err != nil {
		return err
	}
	return writeFile0600(s.path, b)
}

// Deny adds an entry. until is when it stops mattering (zero = never); for a
// jti that is the token's exp, for a sid the latest a token for it could live.
func (s *State) Deny(kind, value string, until time.Time) error {
	if value == "" || (kind != DenyJTI && kind != DenySub && kind != DenySID) {
		return errors.New("authissuer: invalid deny entry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for i, e := range s.doc.Deny {
		if e.Kind == kind && e.Value == value {
			if !e.Until.IsZero() && (until.IsZero() || until.After(e.Until)) {
				s.doc.Deny[i].Until = until
			}
			s.rebuildLocked()
			return s.saveLocked()
		}
	}
	s.doc.Deny = append(s.doc.Deny, denyEntry{kind, value, until})
	s.rebuildLocked()
	return s.saveLocked()
}

// Denied reports whether kind/value is currently denied.
func (s *State) Denied(kind, value string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	for _, e := range s.doc.Deny {
		if e.Kind == kind && e.Value == value {
			return true
		}
	}
	return false
}

// DenyDoc returns the wire body and its ETag.
func (s *State) DenyDoc() (body []byte, etag string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pruneLocked() {
		s.rebuildLocked()
		_ = s.saveLocked()
	}
	return append([]byte(nil), s.body...), s.etag
}

// Grant registers the launch policy for a session.
func (s *State) Grant(sid string, g SessionGrant) error {
	if identity.ReservedIssuerSubject(g.Launcher) {
		return forbidden("issuer protocol subjects cannot launch sessions")
	}
	for audience := range g.Scopes {
		if identity.ReservedEnrollmentAudience(audience) {
			return forbidden("issuer enrollment protocols cannot be delegated")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc.Grants[sid] = g
	return s.saveLocked()
}

// GrantFor returns the live grant for sid.
func (s *State) GrantFor(sid string) (SessionGrant, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.doc.Grants[sid]
	if ok && !s.now().Before(g.Expires) {
		return SessionGrant{}, false
	}
	return g, ok
}

// DropGrant removes a session's grant.
func (s *State) DropGrant(sid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.doc.Grants[sid]; !ok {
		return nil
	}
	delete(s.doc.Grants, sid)
	return s.saveLocked()
}

// TrackMint remembers a token minted from a rotatable parent until it expires.
func (s *State) TrackMint(jti, par string, exp time.Time) error {
	if jti == "" || par == "" {
		return errors.New("authissuer: invalid minted entry")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.doc.Minted = append(s.doc.Minted, mintedEntry{JTI: jti, Par: par, Exp: exp.UTC()})
	return s.saveLocked()
}

// DenyMintedBy denies every unexpired token still tracked as minted from par
// (by jti, until the token's own expiry) and stops tracking them. It returns
// how many tokens were denied. Run it after rotating the parent credential:
// issued tokens are stateless JWTs and would otherwise stay valid until exp.
func (s *State) DenyMintedBy(par string) (int, error) {
	if par == "" {
		return 0, errors.New("authissuer: parent required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	n := 0
	kept := s.doc.Minted[:0]
	for _, m := range s.doc.Minted {
		if m.Par != par {
			kept = append(kept, m)
			continue
		}
		n++
		dup := false
		for _, e := range s.doc.Deny {
			if e.Kind == DenyJTI && e.Value == m.JTI {
				dup = true
				break
			}
		}
		if !dup {
			s.doc.Deny = append(s.doc.Deny, denyEntry{DenyJTI, m.JTI, m.Exp})
		}
	}
	s.doc.Minted = kept
	if n == 0 {
		return 0, nil
	}
	s.rebuildLocked()
	return n, s.saveLocked()
}

// addService records a service token's metadata.
func (s *State) addService(r ServiceRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	if s.doc.Services == nil {
		s.doc.Services = map[string]ServiceRecord{}
	}
	s.doc.Services[r.JTI] = r
	return s.saveLocked()
}

func (s *State) serviceCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	return len(s.doc.Services)
}

// Services lists service token metadata, oldest first. It never holds a token.
func (s *State) Services() []ServiceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	out := make([]ServiceRecord, 0, len(s.doc.Services))
	for _, r := range s.doc.Services {
		r.Scopes = append([]string(nil), r.Scopes...)
		out = append(out, r)
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].IssuedAt.Equal(out[b].IssuedAt) {
			return out[a].IssuedAt.Before(out[b].IssuedAt)
		}
		return out[a].JTI < out[b].JTI
	})
	return out
}
