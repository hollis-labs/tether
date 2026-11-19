# MCP library adoption

CW-20261119-0141 routes app server construction through `go-mcp/server` and
wraps daemon admission through `go-mcp/auth.Provider`. The provider consumes
only the transport-owned verified admission; token verification, live policy
checks, connection binding and stream revocation remain in the transport.
The stdio relay retains its 2025-11-25 protocol pin and upstream capabilities.

The remaining production direct SDK construction is
`internal/mcptransport/handler.go`: `NewStreamableHTTPHandler` selects a server
per admitted view and maintains sessions, timeouts and stream lifetimes.
`go-mcp/transport/http.NewHandler` accepts one fixed server and is stateless,
so substituting it would change authentication and session behavior. The
existing local/remote Host and Origin guards remain required. Test fixtures
may construct raw SDK peers to exercise interoperability.

The existing module version already provides the server and provider seams.
This migration does not upgrade the transport's security defaults or add
protected-resource discovery to Tether's session-bound admission protocol.
