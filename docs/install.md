# Install Tether

Tether ships as three operator-facing binaries:

- `mux` — the main CLI and daemon launcher
- `mux-apikey-helper` — optional helper for storing and resolving provider API
  keys from the local keychain
- `tether_sysop` — the operations GUI (served at `http://localhost:8947/`)

The four supported install paths are Homebrew, GitHub release tarball, source
install, and `go install`.

## Prerequisites

- macOS or Linux
- For source builds: Go `1.26+` and `make`
- For AI provider keychain storage: a local keychain backend supported by
  `go-keyring`, or macOS Keychain

## Option 1: Homebrew

Once the tap formula is published:

```sh
brew install hollis-labs/tap/tether
```

Verify:

```sh
mux --version
mux-apikey-helper --version
```

## Option 2: Release Tarballs

Tagged releases publish tarballs for:

- macOS `arm64`
- macOS `amd64`
- Linux `arm64`
- Linux `amd64`

Each archive contains:

- `mux`
- `mux-apikey-helper`
- `tether_sysop`
- `README.md`
- `LICENSE`
- `install.md`

Example:

```sh
curl -L -o tether.tar.gz \
  https://github.com/hollis-labs/tether/releases/download/v<version>/tether_<version>_darwin_arm64.tar.gz
tar -xzf tether.tar.gz
install -d "$HOME/.local/bin"
install -m 0755 mux "$HOME/.local/bin/"
install -m 0755 mux-apikey-helper "$HOME/.local/bin/"
install -m 0755 tether_sysop "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
```

Releases include per-archive `.tar.gz.sha256` files and a combined
`checksums.txt`.

## Option 3: Source Installs

### Build in place

```sh
git clone git@github.com:hollis-labs/tether.git
cd tether
make build
export PATH="$PWD/bin:$PATH"
```

### Install into a prefix

```sh
git clone git@github.com:hollis-labs/tether.git
cd tether
make install PREFIX="$HOME/.local"
export PATH="$HOME/.local/bin:$PATH"
```

Defaults: `PREFIX=/usr/local`, `BINDIR=$(PREFIX)/bin`.

Override either:

```sh
make install BINDIR="$HOME/bin"
```

Uninstall:

```sh
make uninstall PREFIX="$HOME/.local"
```

### Go-native dev install

If you want the historical "install to `GOBIN`" flow for local development:

```sh
make go-install
```

## Option 4: `go install`

```sh
go install github.com/hollis-labs/tether/cmd/mux@latest
go install github.com/hollis-labs/tether/cmd/mux-apikey-helper@latest
```

## First-Time Setup

Run the guided setup wizard once after install:

```sh
mux init
```

`mux init` is idempotent. It:

1. Creates `~/.tether/{catalog,state,run,logs}` directories.
2. Seeds a starter catalog (global config + 3 CLI providers + example MCP server).
3. Auto-detects installed agent binaries (`claude`, `codex`, `opencode`) and
   offers to record each path — every step is skippable ("set later in
   Settings → Providers").
4. Applies database migrations.
5. Prints a summary of what was written.

Start the daemon and the operations GUI:

```sh
mux daemon start
tether_sysop     # opens the GUI at http://localhost:8947/
```

Verify detection and system health:

```sh
mux detect       # reports found/missing + resolved path for each agent binary
mux doctor       # checks daemon, catalog, migrations, binary paths, permissions
```

Optional: store provider API keys in the local keychain:

```sh
printf '%s\n' "$ANTHROPIC_API_KEY" | mux-apikey-helper set keychain://anthropic/work
printf '%s\n' "$OPENAI_API_KEY"    | mux-apikey-helper set keychain://openai/work
printf '%s\n' "$GEMINI_API_KEY"    | mux-apikey-helper set keychain://gemini/work
```

Then verify the CLI:

```sh
mux sessions list
mux ai providers
```

### Manual catalog setup (fallback)

If you prefer not to use `mux init`, you can bootstrap manually:

```sh
install -d "$HOME/.tether/catalog"
cp -R examples/catalog/* "$HOME/.tether/catalog/"
mux daemon start
```

## Notes

- Tether state lives under `~/.tether/`; the binary can live anywhere on
  `PATH`.
- `mux-apikey-helper` is only required if you use `keychain://...` AI secret
  refs.
- `tether_sysop` is now included in release tarballs and the Homebrew formula.
  Start it any time after `mux daemon start` to access the operations GUI.
- `mux detect` and `mux doctor` reuse the same detection plumbing as `mux init`
  and are safe to re-run at any time.
