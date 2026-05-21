package registry

// callback.go — Sync callback resolvers (T-v060-01-04). The Resolver
// interface is intentionally minimal so v060-02 can drop in http:// and
// mcp:// schemes without disturbing the v1 file:// + cli:// shapes.
//
// Locked decisions in play.
//
//   - D9 — callback transports. v1 ships file:// + cli:// only; http://
//     and mcp:// are explicitly v060-02.
//   - D18 — no raw payload caching. The Resolver returns raw bytes to the
//     Service; the Service parses them, mirrors thin-profile columns via
//     UpdateSelf, and bumps cached_at. The bytes themselves never land in
//     state.db. (See service.go Sync for the call-site.)
//
// Why one resolver per scheme. The two v1 transports have nothing in
// common at the wire level — one reads a local file, the other forks a
// child process. A unified "fetcher" interface that tries to model both
// (retries, auth, streaming) is premature for v1 and would force the
// future http:// resolver to inherit shape it doesn't need. Each Resolver
// owns its own quirks; the Service only knows Scheme() + Resolve().
//
// Size + timeout posture. Both resolvers enforce a 1 MiB cap, but the
// enforcement point differs:
//
//   - FileResolver Stats first, rejects oversize files BEFORE reading
//     (cheap, deterministic).
//   - CLIResolver has no Stat — it reads all stdout then checks length.
//     The 5 s default timeout keeps a runaway subprocess from consuming
//     unbounded memory; an attacker producing exactly maxBytes of output
//     in <5 s gets read-then-rejected. Acceptable for v1.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Resolver fetches a fresh thin-profile payload for a Sync operation. One
// implementation per URI scheme (file://, cli://, future http://, mcp://).
// v1 keeps the interface intentionally minimal — schemes that need richer
// behavior (auth, retries, streaming) wrap a Resolver inside their own type.
type Resolver interface {
	// Scheme reports the URI scheme this resolver handles (e.g. "file",
	// "cli"). Registered on the Service by WithResolver.
	Scheme() string

	// Resolve fetches raw payload bytes for target. target is the full URI
	// (with the scheme:// prefix included) — each Resolver strips its own
	// prefix. Callers MUST honor ctx; both v1 resolvers do.
	Resolve(ctx context.Context, target string) ([]byte, error)
}

// Sentinel errors. Wrapped with %w by both resolvers and Service.Sync so
// callers can branch with errors.Is.

// ErrNoCallback is returned by Service.Sync when the row exists but has
// no callback configured. HTTP-layer maps this to 204 No Content.
var ErrNoCallback = errors.New("registry: sync: row has no callback")

// ErrNoResolver is returned by Service.Sync when no Resolver is registered
// for the row's callback scheme. Operator misconfiguration.
var ErrNoResolver = errors.New("registry: sync: no resolver registered for scheme")

// ErrPayloadInvalid is returned by resolvers when the URI is malformed
// (missing scheme prefix, empty target) or by Service.Sync when the
// resolved payload fails JSON/YAML parse.
var ErrPayloadInvalid = errors.New("registry: sync: payload parse failed")

// ErrPayloadTooLarge is returned by resolvers when the payload exceeds the
// per-resolver byte cap.
var ErrPayloadTooLarge = errors.New("registry: sync: payload exceeds size cap")

// ErrPathOutsideRoot is returned by FileResolver when the target path
// (after canonicalization + symlink resolution) escapes the configured
// catalog root. Symlink-escape attempts surface here.
var ErrPathOutsideRoot = errors.New("registry: file resolver: target outside catalog root")

// defaultResolverMaxBytes is the 1 MiB cap shared by both v1 resolvers.
const defaultResolverMaxBytes = 1 << 20

// defaultCLIResolverTimeout is the 5 s default exec timeout for CLIResolver.
const defaultCLIResolverTimeout = 5 * time.Second

// ─── FileResolver ────────────────────────────────────────────────────────────

// FileResolver reads payloads from disk under a configured catalog root.
// Paths must canonicalize (via filepath.EvalSymlinks) inside catalogRoot;
// symlinks pointing outside the root are rejected with ErrPathOutsideRoot.
//
// File-size enforcement is cheap: os.Stat reports Size() before the read,
// so oversize files never enter memory.
type FileResolver struct {
	catalogRoot string // absolute, canonicalized at construction
	maxBytes    int64
}

// NewFileResolver returns a FileResolver rooted at catalogRoot. The root
// is canonicalized via filepath.Abs + filepath.EvalSymlinks; if the root
// does not yet exist on disk (test fixtures create files later), Abs
// alone is used and the canonical form is recomputed on each Resolve call
// — symlink-escape detection still works because Resolve canonicalizes
// the resolved target against the root's canonical form at call time.
//
// catalogRoot must be non-empty.
func NewFileResolver(catalogRoot string) (*FileResolver, error) {
	if catalogRoot == "" {
		return nil, fmt.Errorf("registry: file resolver: catalog root required")
	}
	abs, err := filepath.Abs(catalogRoot)
	if err != nil {
		return nil, fmt.Errorf("registry: file resolver: abs(%q): %w", catalogRoot, err)
	}
	// Best-effort EvalSymlinks — tolerate missing-root by falling back to
	// the Abs form. Resolve re-canonicalizes each request, so the prefix
	// check stays correct once the root exists.
	if canon, err := filepath.EvalSymlinks(abs); err == nil {
		abs = canon
	}
	return &FileResolver{
		catalogRoot: filepath.Clean(abs),
		maxBytes:    defaultResolverMaxBytes,
	}, nil
}

// Scheme returns "file".
func (f *FileResolver) Scheme() string { return "file" }

// Resolve reads target as a file:// URI. target shape: "file://<abs-path>"
// (D9 picks absolute paths; relative-from-root would force the resolver
// to track two conventions).
//
// Steps:
//  1. Strip "file://" prefix (malformed → wrapped ErrPayloadInvalid).
//  2. Canonicalize via filepath.Abs + filepath.EvalSymlinks (symlinks
//     resolved BEFORE the prefix check so a symlink-escape can be caught).
//  3. Prefix-check against catalogRoot — outside → ErrPathOutsideRoot.
//  4. os.Stat — size > maxBytes → ErrPayloadTooLarge (read avoided).
//  5. os.ReadFile.
//
// ctx is honored: ctx.Err() is checked at the start and after each step.
// Missing-file errors wrap os.ErrNotExist so errors.Is(err, os.ErrNotExist)
// still matches at the caller.
func (f *FileResolver) Resolve(ctx context.Context, target string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const prefix = "file://"
	if !strings.HasPrefix(target, prefix) {
		return nil, fmt.Errorf("registry: file resolver: %w: missing file:// prefix in %q", ErrPayloadInvalid, target)
	}
	path := strings.TrimPrefix(target, prefix)
	if path == "" {
		return nil, fmt.Errorf("registry: file resolver: %w: empty target", ErrPayloadInvalid)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("registry: file resolver: abs(%q): %w", path, err)
	}
	cleaned := filepath.Clean(abs)

	// Re-canonicalize the catalog root each call so an initially-missing
	// root that exists by the time of the first Resolve call still
	// produces a tight prefix.
	root := f.catalogRoot
	if canon, err := filepath.EvalSymlinks(root); err == nil {
		root = filepath.Clean(canon)
	}

	// EvalSymlinks before the prefix check so a symlink pointing outside
	// the root is detected. If the file is missing, surface that wrapped
	// in os.ErrNotExist semantics — callers reading "ErrNotExist" is more
	// useful than a vague ErrPayloadInvalid.
	resolved, err := filepath.EvalSymlinks(cleaned)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("registry: file resolver: %w", err)
		}
		return nil, fmt.Errorf("registry: file resolver: eval symlinks %q: %w", cleaned, err)
	}
	resolved = filepath.Clean(resolved)

	if !pathHasPrefix(resolved, root) {
		return nil, fmt.Errorf("registry: file resolver: %w: %q not under %q", ErrPathOutsideRoot, resolved, root)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	info, err := os.Stat(resolved) //nolint:gosec // G304: path validated against catalogRoot prefix above
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("registry: file resolver: %w", err)
		}
		return nil, fmt.Errorf("registry: file resolver: stat %q: %w", resolved, err)
	}
	if info.Size() > f.maxBytes {
		return nil, fmt.Errorf("registry: file resolver: %w: %d > %d", ErrPayloadTooLarge, info.Size(), f.maxBytes)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(resolved) //nolint:gosec // G304: path validated against catalogRoot prefix above
	if err != nil {
		return nil, fmt.Errorf("registry: file resolver: read %q: %w", resolved, err)
	}
	return data, nil
}

// pathHasPrefix reports whether path is rooted at prefix (treated as a
// directory boundary, so /a/bb is not under /a/b). Both inputs are
// expected to be filepath.Clean'd already.
func pathHasPrefix(path, prefix string) bool {
	if path == prefix {
		return true
	}
	withSep := prefix
	if !strings.HasSuffix(withSep, string(filepath.Separator)) {
		withSep += string(filepath.Separator)
	}
	return strings.HasPrefix(path, withSep)
}

// ─── CLIResolver ─────────────────────────────────────────────────────────────

// CLIResolver executes a command and captures stdout as the payload. The
// command line is parsed with strings.Fields — a naive whitespace split.
// v1 LIMITATION: quoted arguments are NOT supported; binary paths or
// parameters containing spaces cannot be expressed. If a substrate needs
// richer arg passing, wrap the command in a sh -c "..." script and
// reference that script.
//
// Timeout: ctx is wrapped with WithTimeout(c.timeout) so the child cannot
// outlive the resolver's budget. Default 5 s.
//
// Stderr is discarded in v1 — we only need stdout for the thin-profile
// payload, and noisy substrates should not push warnings into our buffer.
type CLIResolver struct {
	timeout  time.Duration
	maxBytes int
}

// NewCLIResolver returns a CLIResolver with the default 5 s timeout and
// 1 MiB payload cap.
func NewCLIResolver() *CLIResolver {
	return &CLIResolver{
		timeout:  defaultCLIResolverTimeout,
		maxBytes: defaultResolverMaxBytes,
	}
}

// Scheme returns "cli".
func (c *CLIResolver) Scheme() string { return "cli" }

// Resolve executes target as a cli:// URI. target shape: "cli://<binary>
// [<arg> ...]" with whitespace-separated args (no quoting v1).
//
// Steps:
//  1. Strip "cli://" prefix (malformed → wrapped ErrPayloadInvalid).
//  2. strings.Fields the rest; empty → wrapped ErrPayloadInvalid.
//  3. exec.CommandContext with a derived ctx that has the timeout applied.
//  4. Run and capture stdout (stderr discarded).
//  5. len(stdout) > maxBytes → wrapped ErrPayloadTooLarge.
//  6. Non-zero exit → wrapped error with exit code (or timeout label).
func (c *CLIResolver) Resolve(ctx context.Context, target string) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	const prefix = "cli://"
	if !strings.HasPrefix(target, prefix) {
		return nil, fmt.Errorf("registry: cli resolver: %w: missing cli:// prefix in %q", ErrPayloadInvalid, target)
	}
	rest := strings.TrimPrefix(target, prefix)
	args := strings.Fields(rest)
	if len(args) == 0 {
		return nil, fmt.Errorf("registry: cli resolver: %w: empty command", ErrPayloadInvalid)
	}

	runCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	var stdout bytes.Buffer
	cmd := exec.CommandContext(runCtx, args[0], args[1:]...) //nolint:gosec // G204: cli:// targets are operator-supplied trust boundary; same as agent-launch resolvers
	cmd.Stdout = &stdout
	cmd.Stderr = nil

	err := cmd.Run()
	out := stdout.Bytes()

	// Size enforcement runs first so an oversized payload from a
	// 0-exit-status command still surfaces as ErrPayloadTooLarge.
	if len(out) > c.maxBytes {
		return nil, fmt.Errorf("registry: cli resolver: %w: %d > %d", ErrPayloadTooLarge, len(out), c.maxBytes)
	}

	if err != nil {
		// Timeout surfaces as ctx.DeadlineExceeded on runCtx; cmd.Run
		// wraps it inside an *exec.ExitError or *exec.Error. Detect by
		// checking the derived ctx.
		if dlErr := runCtx.Err(); errors.Is(dlErr, context.DeadlineExceeded) {
			return nil, fmt.Errorf("registry: cli resolver: cli timed out after %s: %w", c.timeout, dlErr)
		}
		if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
			return nil, fmt.Errorf("registry: cli resolver: command exited with code %d: %w", exitErr.ExitCode(), err)
		}
		return nil, fmt.Errorf("registry: cli resolver: run: %w", err)
	}
	return out, nil
}
