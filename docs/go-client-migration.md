# Go Client Migration

Tether's canonical external Go daemon client is now
[`go-tether-client`](https://github.com/hollis-labs/go-tether-client).

`go-agentmux-client` is the legacy predecessor. New integrations should target
`go-tether-client`, and existing consumers should migrate there before the old
module is archived.

## Why

- Tether is the canonical runtime/product name.
- The default daemon socket path is `unix:~/.tether/run/muxd.sock`.
- The new client includes the typed AI gateway and newer event surfaces that
  were added after the original `go-agentmux-client` shape.

## Surface

`go-tether-client` is the control-plane client apps should integrate with:

- health
- session lifecycle
- attach, wait, input, resize, send turn
- checkpoints
- catalog reads
- event history and event streaming
- messaging routes
- AI providers, models, routes, preview, explain
- AI chat, chat stream, usage, budgets, audit

## Migration Sequence

1. Update imports from `github.com/hollis-labs/go-agentmux-client` to
   `github.com/hollis-labs/go-tether-client`.
2. Replace assumptions about the legacy default socket path with
   `unix:~/.tether/run/muxd.sock`, or pass an explicit listen address.
3. Retest long-lived calls:
   - attach
   - wait
   - send turn
   - AI chat
   - AI chat stream
   - event stream
4. Prefer the typed AI gateway methods in the new client where apps were
   previously using raw HTTP or repo-local client code.

## Tether Status

Tether documents `go-tether-client` as the canonical public Go client. Repo
internal packages still handle CLI/daemon wiring, but external app
integrations should prefer the released client module.
