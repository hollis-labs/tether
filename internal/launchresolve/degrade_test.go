package launchresolve

import (
	"errors"

	"github.com/hollis-labs/agentkit/agentlaunch"
)

// errRegistrarDown is the synthetic outage a faultRegistrar injects.
var errRegistrarDown = errors.New("test: registrar unreachable")

// faultRegistrar is a test double that delegates to an inner Registrar
// until tripped, then fails every call — modeling a directory outage so
// the DegradingRegistrar's cache-fallback path can be exercised.
type faultRegistrar struct {
	inner   agentlaunch.Registrar
	tripped bool
}

func (f *faultRegistrar) Handle(env agentlaunch.RegistryEnvelope) (agentlaunch.RegistryResponse, error) {
	if f.tripped {
		return agentlaunch.RegistryResponse{}, errRegistrarDown
	}
	return f.inner.Handle(env)
}

// openWithFault builds a Registry whose degrading registrar fronts a
// faultRegistrar, so a test can ingest cleanly, prime the last-known-good
// cache with a healthy query, then trip the fault and assert the read
// still resolves from cache. The returned fault handle is the trip switch.
func openWithFault(catalogRoot string) (*Registry, *faultRegistrar, error) {
	root, err := resolveCatalogRoot(catalogRoot)
	if err != nil {
		return nil, nil, err
	}
	inner := agentlaunch.NewInMemoryRegistrar(
		agentlaunch.WithRecordValidator(agentlaunch.PerKindRegistrationValidator),
	)
	fbr := agentlaunch.NewFileBackedRegistrar(root, agentlaunch.WithRegistrar(inner))
	report, err := fbr.IngestCatalog()
	if err != nil {
		return nil, nil, err
	}
	fault := &faultRegistrar{inner: fbr.Registrar()}
	degrading := agentlaunch.NewDegradingRegistrar(fault, agentlaunch.NewLastKnownGoodCache())
	return newRegistry(root, degrading, report), fault, nil
}
