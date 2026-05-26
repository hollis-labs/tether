package client

import (
	"net/http"

	otelprop "github.com/hollis-labs/go-otel/propagation"
)

type tracingRoundTripper struct {
	base http.RoundTripper
}

func (t tracingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	otelprop.InjectHTTP(req.Context(), clone)
	return t.base.RoundTrip(clone)
}

func withTracing(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	clone := *c
	base := clone.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	if _, ok := base.(tracingRoundTripper); ok {
		return &clone
	}
	clone.Transport = tracingRoundTripper{base: base}
	return &clone
}
