package specresolve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/hollis-labs/go-agent-launch/agentlaunch"
)

// ErrCallTransportUnsupported is returned by the http CallResolver when a
// call var source uses a transport it does not implement (only http is
// implemented here — mcp call sources are not used by the launch corpus).
var ErrCallTransportUnsupported = errors.New("specresolve: call transport unsupported")

// envRefPattern matches a ${NAME} environment-variable reference in a call
// target. The S5 corpus authors recall-endpoint targets as
// "${TESSERACT_URL}/v1/recall"; the http CallResolver expands the
// reference from the process environment before dispatching.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// httpCallResolver is the Resolver's agentlaunch.CallResolver for the
// corpus's http `call` var sources (the Tesseract recall endpoint behind
// the recap and memory vars). It performs a plain HTTP GET and returns the
// response body as a string.
//
// D1 (local-first / offline): this resolver does NOT mask failures. An
// unreachable endpoint, an unset ${TESSERACT_URL}, or a non-2xx status all
// return an ordinary (non-permanent) error. The var resolver classifies
// that as a transient source failure, and because every call var in the
// corpus is authored on_error: warn, the var degrades to its empty/fallback
// value and the launch still resolves. The trust gate (run by the var
// resolver BEFORE this) is the only fail-closed step.
type httpCallResolver struct {
	// client is the HTTP client. A per-call timeout is also derived from
	// VarCallRef.Timeout by the var resolver's context, so this client's
	// own timeout is a backstop.
	client *http.Client

	// env resolves ${NAME} references in the call target. Defaults to
	// os.Getenv; overridable for tests.
	env func(string) string
}

// newHTTPCallResolver builds an httpCallResolver with a bounded default
// timeout so an offline launch fails fast rather than hanging.
func newHTTPCallResolver() *httpCallResolver {
	return &httpCallResolver{
		client: &http.Client{Timeout: 15 * time.Second},
		env:    os.Getenv,
	}
}

// ResolveCall implements agentlaunch.CallResolver. It supports the http
// transport only; an mcp transport returns ErrCallTransportUnsupported
// (the var resolver treats that as a transient failure, degraded by the
// var's on_error policy).
func (r *httpCallResolver) ResolveCall(ctx context.Context, ref agentlaunch.VarCallRef) (any, error) {
	if ref.Transport != agentlaunch.CallTransportHTTP {
		return nil, fmt.Errorf("%w: %q (only http is implemented)", ErrCallTransportUnsupported, ref.Transport)
	}

	target := r.expandEnv(ref.Target)
	if strings.Contains(target, "${") {
		// An env reference was left unexpanded — the referenced variable
		// is unset. Surface it as a transient failure so on_error: warn
		// degrades the var rather than dispatching to a broken URL.
		return nil, fmt.Errorf("specresolve: call target %q has an unresolved environment reference", ref.Target)
	}

	u, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("specresolve: call target %q is not a valid URL: %w", target, err)
	}
	q := u.Query()
	for k, v := range ref.Args {
		q.Set(k, fmt.Sprintf("%v", v))
	}
	u.RawQuery = q.Encode()

	method := strings.ToUpper(strings.TrimSpace(ref.Operation))
	if method == "" {
		method = http.MethodGet
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("specresolve: build call request: %w", err)
	}

	resp, err := r.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("specresolve: call %s: %w", u.Host, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("specresolve: read call response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("specresolve: call %s returned status %d", u.Host, resp.StatusCode)
	}
	return strings.TrimRight(string(body), "\n"), nil
}

// expandEnv replaces every ${NAME} reference in s with the value from the
// resolver's environment lookup. An unset variable is left as the literal
// ${NAME} so ResolveCall can detect it and fail cleanly.
func (r *httpCallResolver) expandEnv(s string) string {
	return envRefPattern.ReplaceAllStringFunc(s, func(match string) string {
		name := match[2 : len(match)-1]
		if v := r.env(name); v != "" {
			return v
		}
		return match
	})
}
