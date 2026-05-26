# Install Tether

Tether ships as two operator-facing binaries:

- `mux` — the main CLI and daemon launcher
- `mux-apikey-helper` — optional helper for storing and resolving provider API
  keys from the local keychain

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

Create a starter catalog:

```sh
install -d "$HOME/.tether/catalog"
cp -R examples/catalog/* "$HOME/.tether/catalog/"
```

Start the daemon:

```sh
mux daemon start
mux daemon status
```

Optional: store provider API keys in the local keychain:

```sh
printf '%s\n' "$OPENAI_API_KEY" | mux-apikey-helper set keychain://openai/work
printf '%s\n' "$GEMINI_API_KEY" | mux-apikey-helper set keychain://gemini/work
printf '%s\n' "$ANTHROPIC_API_KEY" | mux-apikey-helper set keychain://anthropic/work
```

Then verify the CLI:

```sh
mux sessions list
mux ai providers
```

## Notes

- Tether state lives under `~/.tether/`; the binary can live anywhere on
  `PATH`.
- `mux-apikey-helper` is only required if you use `keychain://...` AI secret
  refs.
- Sysop ships as a separate binary under `apps/sysop/`; see
  [`apps/sysop/README.md`](../apps/sysop/README.md) for its build/install flow.
