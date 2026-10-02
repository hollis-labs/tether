package mcpadapter

import (
	"net/url"

	"github.com/hollis-labs/tether/internal/config"
	"github.com/hollis-labs/tether/internal/redact"
)

func (p *ClientPool) redactError(err error, entry config.MCPServerEntry) error {
	err = redactUpstreamError(err, entry)
	if err == nil || !p.requireConfinement {
		return err
	}
	// Even a literal endpoint may carry credentials in userinfo or query.
	// Shared-daemon diagnostics never expose either the URL or its values.
	values := []string{entry.URL}
	if endpoint, parseErr := url.Parse(entry.URL); parseErr == nil {
		if endpoint.User != nil {
			values = append(values, endpoint.User.Username())
			if password, ok := endpoint.User.Password(); ok {
				values = append(values, password)
			}
		}
		for _, candidates := range endpoint.Query() {
			values = append(values, candidates...)
		}
	}
	return &redactedError{text: redact.Text(err.Error(), values), err: err}
}
