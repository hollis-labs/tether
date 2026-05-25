package modelcatalog

import (
	"context"
	"time"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
)

// Catalog is Tether's thin wrapper around go-modelsdev. It is the single
// sanctioned place to read provider/model pricing, limits, modalities, and
// capability metadata in runtime code.
type Catalog struct {
	client *modelsdev.Client
}

// New returns a Catalog backed by a models.dev client.
func New(opts ...modelsdev.Option) *Catalog {
	return &Catalog{client: modelsdev.New(opts...)}
}

// Start launches the background refresher. Callers should cancel ctx during
// shutdown.
func (c *Catalog) Start(ctx context.Context) {
	c.client.StartRefresher(ctx)
}

// Refresh performs a synchronous refresh.
func (c *Catalog) Refresh(ctx context.Context) error {
	return c.client.Refresh(ctx)
}

// LastFetchedAt reports the timestamp of the last successful refresh or cache
// load.
func (c *Catalog) LastFetchedAt() time.Time {
	return c.client.LastFetchedAt()
}

// Get returns one model by provider + model id.
func (c *Catalog) Get(providerID, modelID string) (modelsdev.Model, bool) {
	return c.client.Get(providerID, modelID)
}

// List returns every provider/model pair in stable sorted order.
func (c *Catalog) List() []modelsdev.ModelRef {
	return c.client.List()
}

// ListProviders returns every provider in stable sorted order.
func (c *Catalog) ListProviders() []modelsdev.Provider {
	return c.client.ListProviders()
}

// Pricing returns per-million-token input/output USD pricing.
func (c *Catalog) Pricing(providerID, modelID string) (input, output float64, ok bool) {
	m, found := c.client.Get(providerID, modelID)
	if !found {
		return 0, 0, false
	}
	if m.Cost.Input == 0 && m.Cost.Output == 0 {
		return 0, 0, false
	}
	return m.Cost.Input, m.Cost.Output, true
}

// ContextWindow returns the model's context window when known.
func (c *Catalog) ContextWindow(providerID, modelID string) (int, bool) {
	m, found := c.client.Get(providerID, modelID)
	if !found || m.Limit.ContextWindow == 0 {
		return 0, false
	}
	return m.Limit.ContextWindow, true
}

// MaxOutput returns the model's max output token limit when known.
func (c *Catalog) MaxOutput(providerID, modelID string) (int, bool) {
	m, found := c.client.Get(providerID, modelID)
	if !found || m.Limit.MaxOutputTokens == 0 {
		return 0, false
	}
	return m.Limit.MaxOutputTokens, true
}

// Capabilities returns the model capability flags.
func (c *Catalog) Capabilities(providerID, modelID string) (modelsdev.Capabilities, bool) {
	m, found := c.client.Get(providerID, modelID)
	if !found {
		return modelsdev.Capabilities{}, false
	}
	return m.Capabilities, true
}

// Modality returns the model's declared input/output modalities.
func (c *Catalog) Modality(providerID, modelID string) (modelsdev.Modality, bool) {
	m, found := c.client.Get(providerID, modelID)
	if !found {
		return modelsdev.Modality{}, false
	}
	return m.Modality, true
}

// EstimateCost computes an estimated USD cost from prompt + completion token
// counts using the catalog's per-million-token pricing.
func (c *Catalog) EstimateCost(providerID, modelID string, promptTokens, completionTokens int) (float64, bool) {
	in, out, ok := c.Pricing(providerID, modelID)
	if !ok {
		return 0, false
	}
	cost := float64(promptTokens)*in/1_000_000 + float64(completionTokens)*out/1_000_000
	return cost, true
}
