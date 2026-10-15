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
// outpost (a paired device) may ask the issuer to mint for it. The hub
// registers it when it sends the launch, so an outpost's token requests are
// bounded by what the hub authorized for that session and that device, not by
// the device's own Tether scopes.
type SessionGrant struct {
	Device  string              `json:"device"`
	Scopes  map[string][]string `json:"scopes"` // audience -> scopes
	Expires time.Time           `json:"expires"`
}

type stateDoc struct {
	Deny   []denyEntry             `json:"deny"`
	Grants map[string]SessionGrant `json:"grants"`
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
