package api

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/hollis-labs/tether/internal/identity"
)

// legacyGrant lets one legacy session scope satisfy the listed table scopes on
// routes whose table pattern is Path or sits under it (segment boundary).
// Self narrows it to the principal's own resource: the {id} of /sessions/{id}/…
// must be the principal's session, and the {id} of /logical-agents/{id}/… one
// of its server-derived agent addresses. A scope listed nowhere here (terminal,
// maintain, admin, local-only, current-device) is never reachable by mapping.
type legacyGrant struct {
	Path   string
	Scopes []string
	Self   string
}

const (
	selfSession = "session"
	selfAgent   = "logical-agent"
)

var (
	readOnly    = []string{readScope}
	readOperate = []string{readScope, operateScope}
)

// LegacyScopeGrants is the single place the session credential's legacy scopes
// (internal/app/session_identity.go workerScopes) are mapped onto the
// route-to-scope table (CW-20261011-0035). It applies to principals of kind
// "session" only; device and service principals hold table scopes literally.
//
// Deliberately absent, so a session can never reach them through a legacy
// scope: terminal (input/resize/log/attach), maintain (registry writes,
// message purge/redrive, settings, ai usage), admin (/auth/devices, revoke),
// local-only (pairing, bootstrap, MCP/proxy mounts, binding writes), team
// formation/dissolution/membership/assign/delegate/address/cancel, AI chat and
// embeddings, session-group creation and membership, and any control route on
// another session. There is no catalog write route: catalog.write is read-only.
var LegacyScopeGrants = map[string][]legacyGrant{
	"session.write": {
		{Path: "/whoami", Scopes: readOnly},
		{Path: "/auth/context", Scopes: readOnly},
		{Path: "/sessions", Scopes: readOnly},
		{Path: "/sessions", Scopes: []string{operateScope}, Self: selfSession},
		{Path: "/environment", Scopes: readOnly},
		{Path: "/events", Scopes: readOnly},
		{Path: "/routing", Scopes: readOnly},
		{Path: "/docs", Scopes: readOnly},
		{Path: "/ai", Scopes: readOnly},
		{Path: "/logical-agents", Scopes: readOnly},
		{Path: "/logical-agents", Scopes: []string{operateScope}, Self: selfAgent},
		{Path: "/workstreams", Scopes: readOperate},
		{Path: "/session-groups", Scopes: readOnly},
		{Path: "/teams/roster", Scopes: readOnly},
		{Path: "/teams/report_result", Scopes: []string{operateScope}},
	},
	"message.write": {
		{Path: "/messages", Scopes: readOperate},
		{Path: "/broker", Scopes: readOperate},
		{Path: "/groups", Scopes: readOperate},
		{Path: "/channels", Scopes: readOnly},
		{Path: "/mentions", Scopes: readOnly},
	},
	"catalog.write": {
		{Path: "/catalog", Scopes: readOnly},
		{Path: "/docs", Scopes: readOnly},
		{Path: "/registry", Scopes: readOnly},
	},
}

func underPath(pattern, path string) bool {
	return pattern == path || strings.HasPrefix(pattern, path+"/")
}

func pathID(r *http.Request, prefix string) string {
	rest, ok := strings.CutPrefix(r.URL.EscapedPath(), prefix)
	if !ok {
		return ""
	}
	id, _, _ := strings.Cut(rest, "/")
	id, err := url.PathUnescape(id)
	if err != nil {
		return ""
	}
	return id
}

func (g legacyGrant) selfOK(r *http.Request, p identity.Principal) bool {
	switch g.Self {
	case "":
		return true
	case selfSession:
		id := pathID(r, "/sessions/")
		return id == "" || p.SessionID != "" && id == p.SessionID
	case selfAgent:
		id := pathID(r, "/logical-agents/")
		return id != "" && slices.ContainsFunc(p.Addresses, func(a string) bool { return strings.HasSuffix(a, "/"+id) && strings.Contains(a, "/agent/") })
	}
	return false
}

// legacySatisfies reports whether a session principal's legacy scopes cover
// the table scope on this route.
func legacySatisfies(r *http.Request, p identity.Principal, pattern, scope string) bool {
	if p.Kind != "session" {
		return false
	}
	for _, held := range p.Scopes {
		for _, g := range LegacyScopeGrants[held] {
			if underPath(pattern, g.Path) && slices.Contains(g.Scopes, scope) && g.selfOK(r, p) {
				return true
			}
		}
	}
	return false
}
