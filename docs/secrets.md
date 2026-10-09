# Secrets handling (Tether's half)

MCP catalog entries may name a secret instead of containing one. `args:`, `env:`,
`token:` and `url:` all accept:

    keychain://<authority>/<path>       → tether-apikey-helper → macOS `security` CLI
    helper://<helper>/<path>            → the named helper binary
    file:///<absolute path>             → a private file (see below)
    file://~/<path under home>          → the same, relative to your home

Resolution happens in `LoadMCPServers` — the spawn path — and deliberately NOT in
`LoadMCPServerCatalog`, which backs the sysop display and edit surfaces. A catalog
listing therefore shows `keychain://openai/work`, not the key. An unresolvable
reference fails the load rather than spawning an upstream with a blank credential.

- `internal/config/mcp_server.go` — expansion and resolution
- `internal/llm/secrets` — the reference resolver
- `cmd/tether-apikey-helper` — keychain access, service `tether`,
  account `provider-api-key:<authority>/<path>`

Note `tether-apikey-helper` prefers a conventional env var (`OPENAI_API_KEY`,
`ANTHROPIC_API_KEY`) over the keychain. Callers that pass a service environment
through to it must strip reference-valued variables first, or the helper echoes
the reference back as the secret.

## A credential in a private file

`file://` reads the credential from a file, so the catalog YAML does not carry
it. It works in `args:`, `env:`, `token:` and `url:` like the other references,
and is read only at spawn time, never for a catalog listing.

    # ~/.tether/catalog/mcp-servers/tesseract.yaml
    env:
      TESSERACT_MCP_TOKEN: "file://~/.tether/secrets/tesseract.token"

Create the file first, then switch the YAML to the reference, because a file
that cannot be read fails the whole load rather than starting an upstream with a
blank credential:

    install -m 600 /dev/null ~/.tether/secrets/tesseract.token
    printf '%s' "$TOKEN" > ~/.tether/secrets/tesseract.token

Tether checks the file before using it, and refuses, naming the file and the
rule, when:

- the path is relative (use an absolute path or `~/`);
- it is not a regular file (a directory, a device or a FIFO is refused before
  it is opened, so a FIFO cannot hang the load), or is owned by another user;
- group or others can read it (the mode must be 0600 or tighter; `chmod 600`);
- the path goes through a symlink whose target is outside the catalog directory
  and your home directory. A symlink that stays inside either is followed;
- it is empty, or larger than 64 KiB.

Leading and trailing whitespace, such as the newline an editor adds, is
trimmed. Errors never include the file's contents, and the value is scrubbed
from an upstream's stderr tail, tool-call error text, launch records, and the
connect errors stored in a server's status and written to the log, the same way
a keychain value is. A `url:` that came from a reference is scrubbed too, since
a failed connect names the endpoint it tried.

How an MCP reference is recognised:

- `keychain://` and `helper://` resolve only when the catalog YAML authored
  that scheme in the field. A value that becomes either scheme through
  `${VAR}` expansion stays literal and does not invoke a helper. Existing
  operator-authored helper references still expand variables within the
  reference, then resolve at spawn time.

- A value is a file reference only if the catalog YAML says `file://…`. A value
  that merely *becomes* `file://…` through a `${VAR}` stays a literal: a launch's
  caller can set environment variables, and they must not be able to turn an
  ordinary value into a credential-file read.
- `${VAR}` is not expanded inside a `file://` reference, so the path is used as
  written. `file://${VAR}/x` is refused as a relative path, and `file:///a/${X}`
  is a literal absolute path (it fails as not found unless a directory is really
  named `${X}`).
- **`~` still follows `$HOME`**, and a launch's environment can set `$HOME`, so
  `file://~/cred` can end up reading `<another home>/cred` (the 0600, ownership
  and symlink checks still apply, and that other home becomes a root a symlink
  may resolve into). Use an absolute path in a catalog entry to pin the file.
- A literal value written as `file://…` in an MCP entry's `args:`, `env:`,
  `token:` or `url:` is read as a credential file, so one that is not a
  credential file fails the load. No entry in the live catalog has one.

What this does and does not do:

- It keeps the secret out of the catalog YAML (and so out of backups, diffs and
  anything that reads the catalog).
- Put a value reference in `env:` to keep it off argv, or use the explicit
  `token_file` stdio contract below. A `file://` reference in `args:` is still
  replaced by its contents; it does not become a `--token-file` path.
  Current Tesseract and Hadrond authored source accepts `--token-file` and
  `TESSERACT_MCP_TOKEN` / `HADRON_MCP_TOKEN`. Installed adoption must be checked
  separately before changing a live catalog entry.
- It does not hide the value from other processes running as your user. An
  environment is readable from `/proc/<pid>/environ` by any process with the
  same uid, and so is the file itself. Closing that needs the upstreams to run
  outside an agent's reach (CW-20260930-0237, CW-20260930-0253).
- The file and symlink checks catch a misconfigured or misdirected credential.
  They are not a defense against another process running as you.
- Only the credential file itself is checked. A parent directory that others can
  write to is accepted (a 0770 parent passes), and so is a race in which an
  ancestor directory is swapped for a symlink between the checks and the open;
  winning that race needs write access to an ancestor directory.

## Pass a credential path to a stdio upstream

For an upstream that accepts `--token-file PATH`, use this source-only catalog
shape (the file must already contain a credential and have mode 0600):

```yaml
id: tesseract
transport: stdio
command: tesseract
args: [mcp]
token_file: /absolute/private/tesseract.token
```

At every spawn, including reconnects, Tether checks the same owner, regular-file,
permission, size and symlink rules above. It passes `--token-file` and the path
in argv; the file contents are used only for validation and stderr redaction.
Catalog listing retains the authored path and does not read the file. Absolute
paths pin the location; `~/` follows Tether's home. Variables and credential
reference schemes are not expanded in `token_file`.

`token_file` requires stdio and cannot be combined with `token`, `--token`,
`--token=...`, or authored `--token-file` arguments. A credential found in
other arguments or the command/file path fails before spawn. Legacy entries
remain compatible and require a separately authorized catalog change to adopt
this contract. `tether doctor` includes enabled `token_file` entries in its
credential checks. This keeps credentials off argv, not beyond same-user file
access. Catalog edits and subsequent rotation remain separately coordinated
steps; this source change performs neither.

## Populating a keychain entry

`tether-apikey-helper` reads the secret on stdin, so it never appears in a command
line, shell history, or `ps` output:

    printf '%s' "$KEY" | tether-apikey-helper set keychain://openai/work

To confirm a value without printing it, compare digests:

    printf '%s' "$(tether-apikey-helper resolve keychain://openai/work)" \
      | shasum -a 256 | cut -c1-12

### Interop note

`zalando/go-keyring` stores macOS keychain values as
`go-keyring-base64:<base64>` and strips that marker in its own reader. This
helper reads through the `security` CLI, which has no such contract, so it
implements the same decode (`decodeKeyringValue`). Entries written by either
tool are therefore readable by both. Without it, a go-keyring-written entry
reads back as the marker string — credential-shaped, and wrong.

`tether doctor` checks catalog-authored `file://` credentials for enabled MCP
servers before a proxy is started. It uses the spawn-time file checks, reports
unusable files as failures, and never prints their contents or runs secret
helpers. Disabled entries are skipped.
