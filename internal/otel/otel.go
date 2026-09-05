package otel

import (
	"context"
	"log/slog"
	"os"
	"strings"

	feotel "github.com/hollis-labs/go-otel"
)

const (
	disabledEnv = "HOLLIS_OTEL_DISABLED"
	legacyEnv   = "TETHER_OTEL_DISABLED"
)

// builtinDefaultHandler is slog's own default handler, captured before any
// SetDefault can replace it. baseHandler needs to recognize it by identity.
var builtinDefaultHandler = slog.Default().Handler()

func Init(ctx context.Context, serviceName, serviceVersion string) (func(context.Context) error, error) {
	if disabled() {
		return nil, nil
	}

	configureLogging()

	return feotel.Init(ctx,
		feotel.WithServiceName(serviceName),
		feotel.WithServiceVersion(serviceVersion),
		feotel.WithServiceNamespace("hollis"),
		feotel.WithEnvironment(environment()),
	)
}

// configureLogging installs the trace-attributing handler as slog's default.
func configureLogging() {
	slog.SetDefault(slog.New(feotel.NewLogHandler(baseHandler())))
}

// baseHandler returns the handler that trace_id/span_id are layered onto.
//
// It must never be slog's built-in default handler. That handler writes
// through the log package, and slog.SetDefault redirects log.std's output
// back into the new default handler — a cycle on log.std's non-reentrant
// mutex. slog.SetDefault guards against it by skipping the redirect when the
// handler is literally *slog.defaultHandler, but a wrapper hides that type,
// so wrapping it reinstates the cycle: the first log.Printf anywhere in the
// process takes log.std's mutex, reaches the default handler through the
// wrapper, and blocks forever trying to take the same mutex again. That hung
// `mux mcp` before it emitted a single byte of protocol, because app.New logs
// a deprecation warning for the claude-pty provider on the way up.
//
// So the chain terminates in a handler this process owns. stderr, because the
// stdio MCP transport owns stdout.
func baseHandler() slog.Handler {
	if h := slog.Default().Handler(); h != builtinDefaultHandler {
		return h
	}
	return slog.NewTextHandler(os.Stderr, nil)
}

func disabled() bool {
	return envEnabled(disabledEnv) || envEnabled(legacyEnv)
}

func envEnabled(key string) bool {
	v := strings.TrimSpace(os.Getenv(key))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func environment() string {
	if v := strings.TrimSpace(os.Getenv("HOLLIS_ENV")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("APP_ENV")); v != "" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("GO_ENV")); v != "" {
		return v
	}
	return "development"
}
