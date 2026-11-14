package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/registry"
)

// One seat, two addresses (CW-20261114-0013): a catalog agent with urn: binds
// and receives mail on that urn (agt_...), while its sessions know themselves by
// the logical binding target msg://agent/agent-mux/<catalog id>. Access checks
// compared the two literally, so a seat using its logical form could not read
// mail sent to its urn (403), and mail sent to the logical form landed where
// nobody reads it. These helpers treat both as the same principal; the catalog
// urn is canonical.

const logicalAgentPrefix = "msg://agent/agent-mux/"

// canonicalSeat maps a seat's logical form to its catalog urn:; any other
// address is returned unchanged. The catalog decides: an id the catalog defines
// maps to that agent's urn: (or stays itself when it has none) and the registry
// is never asked, because a registry-scope caller can attach any tether id to a
// row it owns. Only an id the catalog does not define falls back to the
// registry's tether id (following a deprecated duplicate's merged_into).
func (s *Server) canonicalSeat(ctx context.Context, urn string) string {
	id, ok := strings.CutPrefix(urn, logicalAgentPrefix)
	if !ok || id == "" || strings.HasPrefix(id, "agt_") || strings.Contains(id, "/") {
		return urn
	}
	if cat := s.seatCatalog(); cat != nil {
		if a, ok := cat.Agents[id]; ok {
			if a.URN != "" {
				return a.URN
			}
			return urn
		}
	}
	if s.Registry != nil {
		if p, err := s.Registry.LookupBy(ctx, registry.KindAgent, id, "tether"); err == nil {
			if p.MergedInto != "" {
				return p.MergedInto
			}
			if p.URN != "" {
				return p.URN
			}
		}
	}
	return urn
}

// seatCatalogTTL bounds how stale the catalog used for seat identity may be; a
// catalog reload is seen within it. Every mailbox request consults it.
const seatCatalogTTL = 5 * time.Second

// seatCatalog is the catalog, cached for seatCatalogTTL (the daemon's loader
// reads every agent file on each Load).
func (s *Server) seatCatalog() *config.Catalog {
	if s.Catalog == nil {
		return nil
	}
	s.seatCatMu.Lock()
	defer s.seatCatMu.Unlock()
	if s.seatCat != nil && time.Since(s.seatCatAt) < seatCatalogTTL {
		return s.seatCat
	}
	cat, err := s.Catalog.Load()
	if err != nil || cat == nil {
		return s.seatCat // keep the last good one
	}
	s.seatCat, s.seatCatAt = cat, time.Now()
	return cat
}

// seatForms is every address of the seat urn names: itself, its catalog urn and
// its logical form.
func (s *Server) seatForms(ctx context.Context, urn string) []string {
	out := []string{urn}
	add := func(u string) {
		if u == "" {
			return
		}
		for _, x := range out {
			if x == u {
				return
			}
		}
		out = append(out, u)
	}
	canon := s.canonicalSeat(ctx, urn)
	add(canon)
	if strings.HasPrefix(canon, logicalAgentPrefix+"agt_") {
		cat := s.seatCatalog()
		if cat != nil {
			for id, a := range cat.Agents {
				if a.URN == canon {
					add(logicalAgentPrefix + id)
				}
			}
		}
		if s.Registry != nil {
			if ids, err := s.Registry.LookupExternalIDsForURN(ctx, canon); err == nil {
				for _, id := range ids {
					if id.Substrate != "tether" {
						continue
					}
					// A tether id that names a catalog agent belongs to that
					// agent's urn: only; a row that merely carries it does not
					// get the seat's logical form.
					if cat != nil {
						if a, ok := cat.Agents[id.ExternalID]; ok && a.URN != canon {
							continue
						}
					}
					add(logicalAgentPrefix + id.ExternalID)
				}
			}
		}
	}
	return out
}

// sameSeat reports whether a and b address the same seat.
func (s *Server) sameSeat(ctx context.Context, a, b string) bool {
	if a == b {
		return true
	}
	return s.canonicalSeat(ctx, a) == s.canonicalSeat(ctx, b)
}

// canonicalizeSeatParams rewrites the as and to query parameters to the seat's
// canonical address, after the claim was verified, so mailbox reads match mail
// stored under the catalog urn whichever form the caller used.
func (s *Server) canonicalizeSeatParams(r *http.Request) {
	q := r.URL.Query()
	changed := false
	for _, key := range []string{"as", "to"} {
		if v := q.Get(key); v != "" {
			if c := s.canonicalSeat(r.Context(), v); c != v {
				q.Set(key, c)
				changed = true
			}
		}
	}
	if changed {
		r.URL.RawQuery = q.Encode()
	}
}
