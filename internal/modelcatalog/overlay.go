package modelcatalog

import (
	"sort"

	"github.com/hollis-labs/go-modelsdev/modelsdev"
)

// Overlay augments a base catalog with configured synthetic models for cases
// where the upstream catalog has no entry yet, such as local OpenAI-compatible
// model ids.
type Overlay struct {
	base  *Catalog
	extra map[string]modelsdev.Model
}

// NewOverlay returns a catalog facade that prefers base metadata when present
// and falls back to configured synthetic entries otherwise.
func NewOverlay(base *Catalog, extra map[string]modelsdev.Model) *Overlay {
	cp := make(map[string]modelsdev.Model, len(extra))
	for k, v := range extra {
		cp[k] = v
	}
	return &Overlay{base: base, extra: cp}
}

func overlayKey(providerID, modelID string) string {
	return providerID + "\x00" + modelID
}

func (o *Overlay) Get(providerID, modelID string) (modelsdev.Model, bool) {
	if o.base != nil {
		if m, ok := o.base.Get(providerID, modelID); ok {
			return m, true
		}
	}
	m, ok := o.extra[overlayKey(providerID, modelID)]
	return m, ok
}

func (o *Overlay) List() []modelsdev.ModelRef {
	var refs []modelsdev.ModelRef
	if o.base != nil {
		refs = append(refs, o.base.List()...)
	}
	seen := map[string]struct{}{}
	for _, ref := range refs {
		seen[overlayKey(ref.ProviderID, ref.ID)] = struct{}{}
	}
	for key, model := range o.extra {
		if _, ok := seen[key]; ok {
			continue
		}
		providerID, modelID := splitOverlayKey(key)
		refs = append(refs, modelsdev.ModelRef{
			ProviderID:   providerID,
			ID:           modelID,
			Name:         model.Name,
			Family:       model.Family,
			OpenWeights:  model.OpenWeights,
			Cost:         model.Cost,
			Limit:        model.Limit,
			Modality:     model.Modality,
			Capabilities: model.Capabilities,
		})
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ProviderID != refs[j].ProviderID {
			return refs[i].ProviderID < refs[j].ProviderID
		}
		return refs[i].ID < refs[j].ID
	})
	return refs
}

func splitOverlayKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '\x00' {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}

func (o *Overlay) Capabilities(providerID, modelID string) (modelsdev.Capabilities, bool) {
	m, ok := o.Get(providerID, modelID)
	if !ok {
		return modelsdev.Capabilities{}, false
	}
	return m.Capabilities, true
}

func (o *Overlay) Modality(providerID, modelID string) (modelsdev.Modality, bool) {
	m, ok := o.Get(providerID, modelID)
	if !ok {
		return modelsdev.Modality{}, false
	}
	return m.Modality, true
}

func (o *Overlay) ContextWindow(providerID, modelID string) (int, bool) {
	m, ok := o.Get(providerID, modelID)
	if !ok || m.Limit.ContextWindow == 0 {
		return 0, false
	}
	return m.Limit.ContextWindow, true
}

func (o *Overlay) MaxOutput(providerID, modelID string) (int, bool) {
	m, ok := o.Get(providerID, modelID)
	if !ok || m.Limit.MaxOutputTokens == 0 {
		return 0, false
	}
	return m.Limit.MaxOutputTokens, true
}

func (o *Overlay) EstimateCost(providerID, modelID string, promptTokens, completionTokens int) (float64, bool) {
	m, ok := o.Get(providerID, modelID)
	if !ok {
		return 0, false
	}
	if m.Cost.Input == 0 && m.Cost.Output == 0 {
		return 0, false
	}
	cost := float64(promptTokens)*m.Cost.Input/1_000_000 + float64(completionTokens)*m.Cost.Output/1_000_000
	return cost, true
}
