package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/hollis-labs/tether/internal/apikeyhelper"
)

var (
	ErrEmptyRef          = errors.New("secret reference is empty")
	ErrUnsupportedScheme = errors.New("unsupported secret reference scheme")
	ErrHelperNotFound    = errors.New("secret helper not found")
	ErrEmptySecret       = errors.New("secret helper returned empty secret")
)

// Ref is a parsed secret reference.
type Ref struct {
	Raw       string
	Scheme    string
	Authority string
	Path      []string
}

// ParseRef validates and parses a secret reference URI.
func ParseRef(raw string) (Ref, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Ref{}, ErrEmptyRef
	}
	scheme, rest, ok := strings.Cut(raw, "://")
	if !ok || scheme == "" || rest == "" {
		return Ref{}, fmt.Errorf("parse secret reference %q: expected <scheme>://<authority>/<path>", raw)
	}
	authority, pathRaw, ok := strings.Cut(rest, "/")
	if !ok || authority == "" || pathRaw == "" {
		return Ref{}, fmt.Errorf("parse secret reference %q: expected authority and path", raw)
	}
	parts := strings.Split(pathRaw, "/")
	for _, part := range parts {
		if part == "" {
			return Ref{}, fmt.Errorf("parse secret reference %q: empty path segment", raw)
		}
	}
	switch scheme {
	case "keychain", "helper":
	default:
		return Ref{}, fmt.Errorf("%w: %s", ErrUnsupportedScheme, scheme)
	}
	return Ref{Raw: raw, Scheme: scheme, Authority: authority, Path: parts}, nil
}

type commandRunner func(context.Context, string, ...string) ([]byte, []byte, error)

// Resolver resolves secret references through mux-apikey-helper or an explicit
// helper named in a helper:// reference.
type Resolver struct {
	resolveDefaultHelper func() string
	resolveNamedHelper   func(string) string
	run                  commandRunner
}

// Option customizes a Resolver.
type Option func(*Resolver)

// WithDefaultHelperPath overrides mux-apikey-helper resolution.
func WithDefaultHelperPath(path string) Option {
	return func(r *Resolver) {
		r.resolveDefaultHelper = func() string { return path }
	}
}

// WithNamedHelperResolver overrides how helper:// names map to executable
// paths. Tests can supply a temp helper directly.
func WithNamedHelperResolver(fn func(string) string) Option {
	return func(r *Resolver) {
		r.resolveNamedHelper = fn
	}
}

// NewResolver returns a secret resolver backed by local helper execution.
func NewResolver(opts ...Option) *Resolver {
	r := &Resolver{
		resolveDefaultHelper: apikeyhelper.ResolvePath,
		resolveNamedHelper:   apikeyhelper.ResolveNamedPath,
		run:                  runCommand,
	}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

// Resolve parses ref and resolves its secret material at runtime.
func (r *Resolver) Resolve(ctx context.Context, ref string) (string, error) {
	parsed, err := ParseRef(ref)
	if err != nil {
		return "", err
	}
	return r.ResolveRef(ctx, parsed)
}

// ResolveRef resolves an already-parsed secret reference.
func (r *Resolver) ResolveRef(ctx context.Context, ref Ref) (string, error) {
	helperPath, err := r.helperPath(ref)
	if err != nil {
		return "", err
	}
	stdout, stderr, err := r.run(ctx, helperPath, "resolve", ref.Raw)
	if err != nil {
		msg := strings.TrimSpace(string(stderr))
		if msg == "" {
			return "", fmt.Errorf("resolve secret via %s: %w", helperLabel(ref), err)
		}
		return "", fmt.Errorf("resolve secret via %s: %s: %w", helperLabel(ref), msg, err)
	}
	secret := strings.TrimSpace(string(stdout))
	if secret == "" {
		return "", fmt.Errorf("resolve secret via %s: %w", helperLabel(ref), ErrEmptySecret)
	}
	return secret, nil
}

func (r *Resolver) helperPath(ref Ref) (string, error) {
	switch ref.Scheme {
	case "keychain":
		if path := r.resolveDefaultHelper(); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("%w: mux-apikey-helper", ErrHelperNotFound)
	case "helper":
		if path := r.resolveNamedHelper(ref.Authority); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("%w: %s", ErrHelperNotFound, ref.Authority)
	default:
		return "", fmt.Errorf("%w: %s", ErrUnsupportedScheme, ref.Scheme)
	}
}

func helperLabel(ref Ref) string {
	if ref.Scheme == "helper" {
		return ref.Authority
	}
	return "mux-apikey-helper"
}

func runCommand(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // helper binary/path is operator-configured input
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
}
