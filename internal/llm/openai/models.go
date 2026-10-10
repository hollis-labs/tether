package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultDiscoveryTimeout bounds a /models discovery call so a hanging
// endpoint cannot hold up daemon start.
const DefaultDiscoveryTimeout = 5 * time.Second

// defaultBaseURL is the OpenAI API root used when a connection sets no base_url.
const defaultBaseURL = "https://api.openai.com/v1"

// maxDiscoveryBody caps how much of a /models response is read.
const maxDiscoveryBody = 8 << 20

// DiscoveryConfig configures a single /models discovery call.
type DiscoveryConfig struct {
	// BaseURL is the connection root, e.g. "https://openrouter.ai/api/v1".
	// Empty means the OpenAI API root.
	BaseURL string
	// APIKey is sent as a bearer token when non-empty.
	APIKey string
	// HTTPClient overrides the transport; nil uses http.DefaultClient.
	HTTPClient *http.Client
	// Timeout bounds the whole call; zero means DefaultDiscoveryTimeout.
	Timeout time.Duration
}

// DiscoverModels lists model ids from an OpenAI-compatible GET /models
// endpoint. The result keeps upstream order with blanks and duplicates
// removed. An empty list is not an error; a non-2xx status, a malformed body
// or a timeout is.
func DiscoverModels(ctx context.Context, cfg DiscoveryConfig) ([]string, error) {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultDiscoveryTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	base := strings.TrimRight(cfg.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/models", nil)
	if err != nil {
		return nil, fmt.Errorf("discover models: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if cfg.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.APIKey)
	}

	client := cfg.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("discover models: timed out after %s", timeout)
		}
		return nil, fmt.Errorf("discover models: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("discover models: unexpected status %d", resp.StatusCode)
	}

	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxDiscoveryBody)).Decode(&body); err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("discover models: timed out after %s", timeout)
		}
		return nil, fmt.Errorf("discover models: decode response: %w", err)
	}

	seen := make(map[string]struct{}, len(body.Data))
	ids := make([]string, 0, len(body.Data))
	for _, m := range body.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}
