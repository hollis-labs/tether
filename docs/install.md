# Install Tether

Tether ships as three operator-facing binaries:

- `tether` — the main CLI and daemon launcher
- `tether-apikey-helper` — optional helper for storing and resolving provider API
  keys from the local keychain
- `tether_sysop` — the operations GUI (served at `http://localhost:8947/`)

The four supported install paths are Homebrew, GitHub release tarball, source
install, and `go install`.

## Prerequisites

- macOS or Linux
- For source builds: the Go version declared in the source checkout’s `go.mod` and `make`
- For Sysop source builds: Node.js 22+ and npm
- For AI provider keychain storage: a local keychain backend supported by
  `go-keyring`, or macOS Keychain

## Option 1: Homebrew

Once the tap formula is published:

```sh
brew install hollis-labs/tap/tether
```

Verify:

```sh
tether --version
tether-apikey-helper --version
```

## Option 2: Release Tarballs

Tagged releases publish tarballs for:

- macOS `arm64`
- macOS `amd64`
- Linux `arm64`
- Linux `amd64`

Each archive contains:

- `tether`
- `tether-apikey-helper`
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
install -m 0755 tether "$HOME/.local/bin/"
install -m 0755 tether-apikey-helper "$HOME/.local/bin/"
install -m 0755 tether_sysop "$HOME/.local/bin/"
export PATH="$HOME/.local/bin:$PATH"
```

Releases publish `checksums.txt` with a SHA-256 digest for every archive.
The local packaging script also produces per-archive `.sha256` files; the
release workflow uploads the combined checksum file.

## Option 3: Source Installs

### Build in place

```sh
git clone git@github.com:hollis-labs/tether.git
cd tether
make release-build
export PATH="$PWD/bin:$PWD/apps/sysop:$PATH"
```

### Install into a prefix

```sh
git clone git@github.com:hollis-labs/tether.git
cd tether
make install PREFIX="$HOME/.local"
GOBIN="$HOME/.local/bin" make sysop-install
export PATH="$HOME/.local/bin:$PATH"
```

Defaults: `PREFIX=/usr/local`, `BINDIR=$(PREFIX)/bin`.

Override either:

```sh
make install BINDIR="$HOME/bin"
```

Uninstall the two core binaries installed by `make install`:

```sh
make uninstall PREFIX="$HOME/.local"
```

Sysop is installed separately into `GOBIN`; the root uninstall target does not
remove `tether_sysop`.

### Go-native dev install

If you want the historical "install to `GOBIN`" flow for local development:

```sh
make release-install
```

## Option 4: `go install` (core binaries)

```sh
go install github.com/hollis-labs/tether/cmd/tether@latest
go install github.com/hollis-labs/tether/cmd/tether-apikey-helper@latest
```

For Sysop, use a release archive or build its embedded frontend with the source
install commands above. A bare Go install cannot generate those frontend assets.

## First-Time Setup

For an independently managed Linux worker with checksummed, retained runtimes,
see [the worker service guide](worker-service.md).

Run the guided setup wizard once after install:

```sh
tether init
```

`tether init` is idempotent. It:

1. Creates `~/.tether/{catalog,state,run,logs}` directories.
2. Seeds a starter catalog (global config, CLI provider entries, and example MCP servers).
3. Auto-detects installed agent binaries (`claude`, `codex`, `opencode`, `agy`) and
   offers to record each path — every step is skippable ("set later in
   Settings → Providers").
4. Applies database migrations.
5. Prints a summary of what was written.

Start the daemon and the operations GUI:

```sh
tether daemon start
tether_sysop     # opens the GUI at http://localhost:8947/
```

Verify detection and system health:

```sh
tether detect       # reports found/missing + resolved path for each agent binary
tether doctor       # checks daemon, catalog, migrations, binary paths, permissions
```

Optional: store provider API keys in the local keychain:

```sh
printf '%s\n' "$ANTHROPIC_API_KEY" | tether-apikey-helper set keychain://anthropic/work
printf '%s\n' "$OPENAI_API_KEY"    | tether-apikey-helper set keychain://openai/work
printf '%s\n' "$GEMINI_API_KEY"    | tether-apikey-helper set keychain://gemini/work
```

Then verify the CLI:

```sh
tether sessions list
tether ai providers
```

### Manual catalog setup (fallback)

If you prefer not to use `tether init`, you can bootstrap manually:

```sh
install -d "$HOME/.tether/catalog"
cp -R examples/catalog/* "$HOME/.tether/catalog/"
tether daemon start
```

## Notes

- Tether state lives under `~/.tether/`; the binary can live anywhere on
  `PATH`.
- `tether-apikey-helper` is only required if you use `keychain://...` AI secret
  refs.
- `tether_sysop` is now included in release tarballs and the Homebrew formula.
  Start it any time after `tether daemon start` to access the operations GUI.
- `tether detect` and `tether doctor` reuse the same detection plumbing as `tether init`
  and are safe to re-run at any time.
