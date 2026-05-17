package specresolve

import (
	"context"
	"fmt"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
)

// catalogTrustTokens is the closed allow-list of TrustGate.Trust tokens the
// Resolver's TrustAuthorizer authorizes. These are exactly the tokens the
// S5 launch-spec corpus authors on its call/cmd var sources
// (testdata/launch-specs/launch-assembly.yaml):
//
//   - catalog-recall-endpoint — the http `call` sources for the recap and
//     memory vars (the Tesseract recall endpoint).
//   - catalog-git-readonly    — the `cmd` sources for the history and
//     status vars (read-only `git log` / `git status`).
//   - catalog-skill-index     — the `cmd` source for the skills var
//     (the `mux skills index` subcommand).
//
// A gated var source carrying any other trust token is DENIED. D6(c) is
// fail-closed: the authorizer authorizes a known, catalog-authored token,
// it does not authorize arbitrary input. Adding a new gated source to the
// corpus is therefore a deliberate two-place change (corpus + this list).
var catalogTrustTokens = map[string]struct{}{
	"catalog-recall-endpoint": {},
	"catalog-git-readonly":    {},
	"catalog-skill-index":     {},
}

// CatalogTrustTokens returns a copy of the trust-token allow-list the
// Resolver authorizes. It is exported for tests and for callers auditing
// which gated sources the resolver will run.
func CatalogTrustTokens() []string {
	out := make([]string, 0, len(catalogTrustTokens))
	for tok := range catalogTrustTokens {
		out = append(out, tok)
	}
	return out
}

// catalogTrustAuthorizer is the Resolver's agentlaunch.TrustAuthorizer
// (D6c). It authorizes a gated call/cmd var source iff the source's
// TrustGate.Trust token is in the catalog allow-list. An empty or unknown
// token is denied; the var resolver turns a denial into a permanent
// failure, so a typo'd or unexpected token fails the launch loudly rather
// than silently running an unauthorized source.
type catalogTrustAuthorizer struct{}

// Authorize implements agentlaunch.TrustAuthorizer.
func (catalogTrustAuthorizer) Authorize(_ context.Context, varName string, src agentlaunch.VarSource) (agentlaunch.TrustDecision, error) {
	gate := sourceGate(src)
	if gate.Trust == "" {
		return agentlaunch.TrustDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("var %q gated source carries no trust token", varName),
		}, nil
	}
	if _, ok := catalogTrustTokens[gate.Trust]; !ok {
		return agentlaunch.TrustDecision{
			Allowed: false,
			Reason: fmt.Sprintf("var %q gated source trust token %q is not in the catalog allow-list",
				varName, gate.Trust),
		}, nil
	}
	return agentlaunch.TrustDecision{Allowed: true}, nil
}

// sourceGate extracts the TrustGate from whichever call/cmd branch of a
// VarSource is populated. A literal/file source carries no gate and yields
// the zero gate (which Authorize rejects — but the var resolver never gates
// literal/file sources, so that path is unreachable for those kinds).
func sourceGate(src agentlaunch.VarSource) agentlaunch.TrustGate {
	switch src.Kind {
	case agentlaunch.VarSourceCall:
		if src.Call != nil {
			return src.Call.Gate
		}
	case agentlaunch.VarSourceCmd:
		if src.Cmd != nil {
			return src.Cmd.Gate
		}
	}
	return agentlaunch.TrustGate{}
}
