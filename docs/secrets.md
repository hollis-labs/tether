# Secrets handling (Tether's half)

MCP catalog entries may name a secret instead of containing one. `args:`, `env:`,
`token:` and `url:` all accept:

    keychain://<authority>/<path>       → mux-apikey-helper → macOS `security` CLI
    helper://<helper>/<path>            → the named helper binary

Resolution happens in `LoadMCPServers` — the spawn path — and deliberately NOT in
`LoadMCPServerCatalog`, which backs the sysop display and edit surfaces. A catalog
listing therefore shows `keychain://openai/work`, not the key. An unresolvable
reference fails the load rather than spawning an upstream with a blank credential.

- `internal/config/mcp_server.go` — expansion and resolution
- `internal/llm/secrets` — the reference resolver
- `cmd/mux-apikey-helper` — keychain access, service `tether`,
  account `provider-api-key:<authority>/<path>`

Note `mux-apikey-helper` prefers a conventional env var (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`) over the keychain. Callers that pass a service environment
through to it must strip reference-valued variables first, or the helper echoes
the reference back as the secret.

## Populating a keychain entry

`mux-apikey-helper` reads the secret on stdin, so it never appears in a command
line, shell history, or `ps` output:

    printf '%s' "$KEY" | mux-apikey-helper set keychain://openai/work

To confirm a value without printing it, compare digests:

    printf '%s' "$(mux-apikey-helper resolve keychain://openai/work)" \
      | shasum -a 256 | cut -c1-12

### Interop note

`zalando/go-keyring` stores macOS keychain values as
`go-keyring-base64:<base64>` and strips that marker in its own reader. This
helper reads through the `security` CLI, which has no such contract, so it
implements the same decode (`decodeKeyringValue`). Entries written by either
tool are therefore readable by both. Without it, a go-keyring-written entry
reads back as the marker string — credential-shaped, and wrong.
