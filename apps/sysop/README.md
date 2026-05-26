# TetherSysop

Agent Ops — the Tether Agent Operations dashboard

A **Sysop UI** application: a React frontend built on
[`@hollis-labs/sysop-ui`](https://github.com/hollis-labs/sysop-ui) served by a
Go binary through the [`go-webui`](https://github.com/hollis-labs/go-webui)
embed harness. Scaffolded by `folio new sysop-ui`. Served at `/operations`.

## Layout

```
cmd/tether_sysop/   Go entrypoint — HTTP server + /api
internal/webui/             //go:embed all:dist + the go-webui handler
frontend/                   Vite + React frontend (the Sysop UI)
  src/App.tsx               app shell — nav rail + page header
  src/pages/                one page per screen
  src/api/                  same-origin API client + typed context
```

The frontend builds into `internal/webui/dist/`, which the Go binary
embeds — so a single binary serves both the API and the UI.

## Prerequisites

- Go 1.26.1+
- Node.js 20+ / npm

## Develop

Two processes during development:

```sh
make run      # Go server on :8947 (serves /api and the last UI build)
make ui-dev   # Vite dev server with hot reload — proxies /api to :8947
```

Open the Vite dev server URL — the app is served under `/operations/`,
not the root path.

## Build a release binary

```sh
make all      # ui-build (vite → internal/webui/dist) then build
./tether_sysop
```

The Agent Ops UI is then served at <http://localhost:8947/operations/>.
Cerberus manages this as the `tether-sysop-dev` resource (port 8947).
Before the first `make ui-build`, `go-webui` serves a "not built"
placeholder in place of the app.

| Command | What it does |
|---|---|
| `make ui-build` | Build the frontend into `internal/webui/dist` |
| `make ui-dev` | Run the Vite dev server (hot reload) |
| `make build` | Build the Go binary |
| `make all` | `ui-build` then `build` |
| `make run` | Build and run the server |
| `make install` | Build the embedded UI + install `tether_sysop` to `$GOBIN` |
| `make test` / `make vet` | Go test / vet |

## Adding a page

A page is generic kit chrome plus app-specific content. Add a component
under `frontend/src/pages/`, then wire it into `frontend/src/App.tsx`
(extend the `nav` array and the active-route switch). Add API endpoints
to `frontend/src/api/client.ts`. See the
[`@hollis-labs/sysop-ui` README](https://github.com/hollis-labs/sysop-ui)
for the `PageHeader` / `DataTable` / `SummaryCards` composition pattern.

## Dependencies

- **`@hollis-labs/sysop-ui`** (`v0.9.0`) — the React
  kit + canonical theme. Consumed from the published package registry and
  pinned to a release version. For local kit development, override it with a
  working copy from `frontend/`:
  `npm install file:<path-to>/libs/sysop-ui`.
- **`github.com/hollis-labs/go-webui`** (`v0.1.0`) —
  the SPA-serving harness.

## License

MIT — see [LICENSE](./LICENSE).
