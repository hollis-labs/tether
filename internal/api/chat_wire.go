package api

import (
	"net/http"
	"slices"
	"sync"
)

// The chat wire (CW-20261118-0011): libs/agent/chat's conversation handler for the agents this daemon runs, mounted
// at /chat/v1 when the daemon's chat_wire flag is on. With the flag off nothing here is registered and neither scope
// table changes.

// chatWirePrefix is where the handler is mounted; its own routes are below it (chatwire.Prefix).
const chatWirePrefix = "/chat/v1"

// registerChatWireRoutes mounts Deps.ChatWire, when there is one.
func (s *Server) registerChatWireRoutes(router *http.ServeMux) {
	if s.ChatWire == nil {
		return
	}
	router.Handle("/chat/v1/", http.StripPrefix(chatWirePrefix, s.ChatWire))
}

// ChatWireRouteScopes is the chat wire's route policy. It is kept out of RemoteRouteScopes and LocalRouteScopes until
// the daemon mounts the wire (EnableChatWireScopes), so with the flag off both tables, and the answer to every
// request, are what they were. The handler's own Authorize then decides per conversation: attaching as a controller
// needs operate even on the attachments route, whose table scope is read because a viewer attaches there too.
var ChatWireRouteScopes = []RouteScope{
	routeScope("/chat/v1/", "/chat/v1/agents/{agent}/conversation", map[string]string{"POST": readScope}),
	routeScope("/chat/v1/", "/chat/v1/agents/{agent}/conversation/reset", map[string]string{"POST": operateScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}", map[string]string{"GET": readScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/attachments", map[string]string{"POST": readScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/attachments/{attachment}", map[string]string{"DELETE": readScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/events", map[string]string{"GET": readScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/lease", map[string]string{"POST": operateScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/lease/takeover", map[string]string{"POST": operateScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/turns", map[string]string{"POST": operateScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/runs/{run}/cancel", map[string]string{"POST": operateScope}),
	routeScope("/chat/v1/", "/chat/v1/conversations/{id}/approvals/{approval}", map[string]string{"POST": operateScope}),
}

var chatWireScopes sync.Once

// EnableChatWireScopes adds ChatWireRouteScopes to both route tables, once. The daemon calls it at startup, before it
// serves, when it mounts the chat wire.
func EnableChatWireScopes() {
	chatWireScopes.Do(func() {
		RemoteRouteScopes = append(slices.Clip(RemoteRouteScopes), ChatWireRouteScopes...)
		LocalRouteScopes = append(slices.Clip(LocalRouteScopes), slices.Clone(ChatWireRouteScopes)...)
	})
}
