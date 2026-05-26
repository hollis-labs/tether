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

func Init(ctx context.Context, serviceName, serviceVersion string) (func(context.Context) error, error) {
	if disabled() {
		return nil, nil
	}

	slog.SetDefault(slog.New(feotel.NewLogHandler(slog.Default().Handler())))

	return feotel.Init(ctx,
		feotel.WithServiceName(serviceName),
		feotel.WithServiceVersion(serviceVersion),
		feotel.WithServiceNamespace("hollis"),
		feotel.WithEnvironment(environment()),
	)
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
