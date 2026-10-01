# Secrets handling (Tether's half)

MCP catalog entries may name a secret instead of containing one. `args:`, `env:`,
`token:` and `url:` all accept:

    keychain://<authority>/<path>       → mux-apikey-helper → macOS `security` CLI
    helper://<helper>/<path>            → the named helper binary
    file:///<absolute path>             → a private file (see below)
    file://~/<path under home>          → the same, relative to your home

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

## A credential in a private file

`file://` reads the credential from a file, so the catalog YAML does not carry
it. It works in `args:`, `env:`, `token:` and `url:` like the other references,
and is read only at spawn time, never for a catalog listing.

    # ~/.tether/catalog/mcp-servers/tesseract.yaml
    env:
      TESSERACT_TOKEN: "file://~/.tether/secrets/tesseract.token"

Create the file first, then switch the YAML to the reference, because a file
that cannot be read fails the whole load rather than starting an upstream with a
blank credential:

    install -m 600 /dev/null ~/.tether/secrets/tesseract.token
    printf '%s' "$TOKEN" > ~/.tether/secrets/tesseract.token

Tether checks the file before using it, and refuses, naming the file and the
rule, when:

- the path is relative (use an absolute path or `~/`);
- it is not a regular file, or is owned by another user;
- group or others can read it (the mode must be 0600 or tighter; `chmod 600`);
- the path goes through a symlink whose target is outside the catalog directory
  and your home directory. A symlink that stays inside either is followed;
- it is empty, or larger than 64 KiB.

Leading and trailing whitespace, such as the newline an editor adds, is
trimmed. Errors never include the file's contents, and the value is scrubbed
from an upstream's stderr tail, tool-call error text and launch records the
same way a keychain value is. A literal value that begins with `file://` in an
MCP server entry's `args:`, `env:`, `token:` or `url:` is now read as a
credential file.

What this does and does not do:

- It keeps the secret out of the catalog YAML (and so out of backups, diffs and
  anything that reads the catalog).
- **Put it in `env:`, not `args:`, to keep it off the command line.** A
  `file://` reference in `args:` is replaced by the value, so the upstream's
  `/proc/<pid>/cmdline` still shows it, exactly as a literal would. That helps
  only for an upstream that reads its credential from the environment. An
  upstream whose only input is a flag (today `tesseract mcp --token` and
  `hadrond mcp --token`) keeps the value on its command line until that
  upstream accepts the environment or a file.
- It does not hide the value from other processes running as your user. An
  environment is readable from `/proc/<pid>/environ` by any process with the
  same uid, and so is the file itself. Closing that needs the upstreams to run
  outside an agent's reach (CW-20260930-0237, CW-20260930-0253).
- The file and symlink checks catch a misconfigured or misdirected credential.
  They are not a defence against another process running as you.

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
