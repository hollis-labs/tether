package app

import (
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/hollis-labs/tether/internal/config"
)

// persistedIdentityTTL bounds how stale the persisted identity mode may be when
// a launch is decided. It is short on purpose: the rule below is a safety net
// for the gap between `tether identity flip` and the daemon restart.
const persistedIdentityTTL = 3 * time.Second

// persistedIdentity reads the identity block of the catalog's global.yaml, the
// mode the operator has persisted, behind a short TTL cache. The daemon applies
// modes once at start; this is how a running daemon sees a flip before restart.
// The zero value is ready to use. Errors are cached for the TTL too, so a
// broken catalog is not re-read on every launch.
type persistedIdentity struct {
	mu   sync.Mutex
	at   time.Time
	cfg  config.IdentityConfig
	err  error
	ttl  time.Duration                                    // zero = persistedIdentityTTL
	now  func() time.Time                                 // nil = time.Now
	read func(root string) (config.IdentityConfig, error) // nil = config.LoadIdentity
	hits int                                              // reads performed (tests)
	// warned is set once the missing-global.yaml warning has been logged.
	warned bool
}

func (p *persistedIdentity) get(root string) (config.IdentityConfig, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now
	if p.now != nil {
		now = p.now
	}
	ttl := p.ttl
	if ttl == 0 {
		ttl = persistedIdentityTTL
	}
	if !p.at.IsZero() && now().Sub(p.at) < ttl {
		return p.cfg, p.err
	}
	read := config.LoadIdentity
	if p.read != nil {
		read = p.read
	}
	p.cfg, p.err = read(root)
	p.at = now()
	p.hits++
	if errors.Is(p.err, os.ErrNotExist) && !p.warned {
		// A catalog root with no global.yaml: the daemon falls back to the mode it
		// started with. Say so once, so a deleted file is visible.
		p.warned = true
		log.Printf("WARNING: catalog root %q has no global.yaml; the persisted identity mode cannot be read, so only the mode this daemon started with applies to unbounded launches", root)
	}
	return p.cfg, p.err
}

// refuseUnboundedWhenEnforced is the daemon-side rule behind the enforce flip
// (CW-20261016-0231, architect ruling anonymous_worker_launch_scopes item 3):
// once the PERSISTED configured mode is enforce, a launch that nothing bounds
// is refused immediately, even though this daemon process still runs the mode it
// started with. Without it, launches between `tether identity flip` and the
// restart keep minting credentials with the full profile ceiling.
//
//   - anonymous_external is refused when the local_api point or the global mode
//     is persisted as enforce (the same condition under which the listener will
//     reject anonymous requests after the restart);
//   - internal_unattributed is refused when the global mode is persisted as
//     enforce. A single point such as teams being enforce does not trigger it:
//     the hub has run teams=enforce for days, and its recovery launches have no
//     origin.
//
// Launches a principal or an origin session bounded are never affected, and
// never read the catalog. The mode the daemon started with counts too, so
// enforce at start is never loosened by what is on disk. If the catalog exists
// but cannot be read when an unbounded launch is decided, the launch is refused:
// it fails closed. (A root with no global.yaml at all, as in an in-process
// service built from an in-memory catalog, has no persisted mode to consult.)
func (s *Service) refuseUnboundedWhenEnforced(a Attribution) error {
	if a != AttributionAnonymous && a != AttributionInternalUnattributed {
		return nil
	}
	// The mode the daemon started with always counts: enforce at start is never
	// loosened by what is on disk now.
	var configs []config.IdentityConfig
	if s.cat() != nil {
		configs = append(configs, s.cat().Global.Identity)
	}
	if s.CatalogRoot != "" {
		persisted, err := s.persisted.get(s.CatalogRoot)
		switch {
		case err == nil:
			configs = append(configs, persisted)
		case errors.Is(err, os.ErrNotExist):
			// No global.yaml at this root (an in-process service built from an
			// in-memory catalog): there is no persisted mode to consult.
		default:
			return fmt.Errorf("%w: the persisted identity mode cannot be read, so an unbounded launch (%s) is not allowed: %w", ErrLaunchRefused, a, err)
		}
	}
	for _, cfg := range configs {
		enforced := cfg.EffectiveMode() == "enforce"
		if a == AttributionAnonymous {
			enforced = enforced || cfg.ModeFor("local_api") == "enforce"
		}
		if enforced {
			return fmt.Errorf("%w: identity is enforce, so a launch with no bounding authority (%s) is refused (it would keep the full profile scopes)", ErrLaunchRefused, a)
		}
	}
	return nil
}
