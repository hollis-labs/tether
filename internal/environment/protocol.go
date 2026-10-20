package environment

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
)

// Protocol changes for breaking remote API changes; additive fields/groups do
// not change it. Keep versioned wire conformance tests alongside the surface.
const Protocol = 1
const ProtocolHeader = "Tether-Protocol"

type ProtocolError struct {
	Code             string `json:"code"`
	Message          string `json:"message"`
	RequiredProtocol int    `json:"required_protocol"`
	UpdateHint       string `json:"update_hint"`
}

// thirdPartyRead reports the requests that non-Tether programs make with a
// plain HTTP client and, at most, a bearer: go-auth fetches the issuer's JWKS and
// polls its deny-list, and any app publishes events with POST /events (the
// libs mesh/eventbus client). They cannot send the Tether-Protocol header, so the
// gate must not demand it. The closed list is exactly: GET/HEAD the JWKS, GET the
// deny-list, POST /events. The deny-list and JWKS reads are public; POST /events
// stays behind authentication and its own versioned contract (the libs ingest
// schema). Every other route (notably POST /auth/token) still needs the header.
//
// The hub's remote MCP gateway (exactly POST, GET and DELETE /mcp for a bound agent token, see internal/daemon/remote_mcp.go)
// is likewise served without the header, but it is exempt by sitting outside this gate, not by an entry here: Tether-Protocol
// is a version gate for the Tether API, not authentication, and MCP clients negotiate their own version through initialize
// and MCP-Protocol-Version, so they cannot be asked to send it. Any other path under /mcp (for example /mcp/anything) and
// every other method still go through the ordinary remote chain and still need the header.
func thirdPartyRead(r *http.Request) bool {
	switch r.URL.Path {
	case "/.well-known/jwks.json":
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case "/auth/denylist":
		return r.Method == http.MethodGet
	case "/events":
		return r.Method == http.MethodPost
	}
	return false
}

// ProtocolGate keeps legacy local callers compatible when require=false.
// Remote listeners and new versioned stream routes use require=true. Health
// and descriptor remain bootstrap-readable regardless of a client's version,
// as are the issuer reads in thirdPartyRead.
func ProtocolGate(require bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" || r.URL.Path == DescriptorPath || thirdPartyRead(r) {
			next.ServeHTTP(w, r)
			return
		}
		parsedQuery, queryErr := url.ParseQuery(r.URL.RawQuery)
		headers, query := r.Header.Values(ProtocolHeader), parsedQuery["protocol"]
		if len(headers) == 0 && len(query) == 0 && !require && queryErr == nil {
			next.ServeHTTP(w, r)
			return
		}
		valid := queryErr == nil && len(headers) <= 1 && len(query) <= 1 && len(headers)+len(query) > 0
		for _, values := range [][]string{headers, query} {
			for _, value := range values {
				n, err := strconv.Atoi(value)
				valid = valid && err == nil && n == Protocol && value == strconv.Itoa(Protocol)
			}
		}
		if valid {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(struct {
			Error ProtocolError `json:"error"`
		}{ProtocolError{Code: "protocol_mismatch", Message: "client and environment protocols must match", RequiredProtocol: Protocol, UpdateHint: "Fetch " + DescriptorPath + " and update the client or environment to the same protocol."}})
	})
}
