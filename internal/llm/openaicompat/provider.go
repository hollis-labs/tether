package openaicompat

import (
	"context"
	"net/http"

	llmopenai "github.com/hollis-labs/tether/internal/llm/openai"
)

// Config configures one OpenAI-compatible provider instance. Secret resolution is
// optional; when omitted the adapter sends no Authorization header.
type Config struct {
	ResolveAPIKey func(context.Context) (string, error)
	BaseURL       string
	HTTPClient    *http.Client
}

// New returns an OpenAI-compatible provider backed by the official OpenAI Go
// SDK, with unauthenticated local-server mode enabled when ResolveAPIKey is nil.
func New(cfg Config) *llmopenai.Provider {
	return llmopenai.New(llmopenai.Config{
		ResolveAPIKey:        cfg.ResolveAPIKey,
		BaseURL:              cfg.BaseURL,
		HTTPClient:           cfg.HTTPClient,
		AllowUnauthenticated: true,
	})
}
