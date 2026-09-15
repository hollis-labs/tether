package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/hollis-labs/tether/internal/settings"
)

// SettingsClient provides typed access to the daemon's settings cascade HTTP surface (CW-20260914-0042).
type SettingsClient struct {
	c *Client
}

// Settings returns a SettingsClient bound to this Client.
func (c *Client) Settings() *SettingsClient {
	return &SettingsClient{c: c}
}

// GetEffectiveOnboarding queries the daemon for effective onboarding settings
// resolved across the Global > Project > User cascade.
func (sc *SettingsClient) GetEffectiveOnboarding(ctx context.Context, projectID, userID string) (settings.OnboardingSettings, error) {
	u, err := url.Parse(sc.c.baseURL + "/settings/onboarding")
	if err != nil {
		return settings.OnboardingSettings{}, err
	}
	q := u.Query()
	if projectID != "" {
		q.Set("project", projectID)
	}
	if userID != "" {
		q.Set("user", userID)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return settings.OnboardingSettings{}, err
	}
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return settings.OnboardingSettings{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return settings.OnboardingSettings{}, readRegistryError(resp)
	}
	var out settings.OnboardingSettings
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return settings.OnboardingSettings{}, fmt.Errorf("decode effective onboarding settings: %w", err)
	}
	return out, nil
}

// GetOnboarding fetches explicit onboarding settings for a specific scope tier.
func (sc *SettingsClient) GetOnboarding(ctx context.Context, scope settings.Scope, scopeID string) (settings.OnboardingSettings, error) {
	path := "/settings/onboarding/" + string(scope)
	u, err := url.Parse(sc.c.baseURL + path)
	if err != nil {
		return settings.OnboardingSettings{}, err
	}
	if scopeID != "" {
		q := u.Query()
		q.Set("scope_id", scopeID)
		u.RawQuery = q.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return settings.OnboardingSettings{}, err
	}
	resp, err := sc.c.http.Do(req)
	if err != nil {
		return settings.OnboardingSettings{}, wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return settings.OnboardingSettings{}, readRegistryError(resp)
	}
	var out settings.OnboardingSettings
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return settings.OnboardingSettings{}, fmt.Errorf("decode onboarding settings: %w", err)
	}
	return out, nil
}

// SetOnboarding saves explicit onboarding settings for a specific scope tier.
func (sc *SettingsClient) SetOnboarding(ctx context.Context, scope settings.Scope, scopeID string, ob settings.OnboardingSettings) error {
	path := "/settings/onboarding/" + string(scope)
	u, err := url.Parse(sc.c.baseURL + path)
	if err != nil {
		return err
	}
	if scopeID != "" {
		q := u.Query()
		q.Set("scope_id", scopeID)
		u.RawQuery = q.Encode()
	}

	body, err := json.Marshal(ob)
	if err != nil {
		return fmt.Errorf("marshal onboarding settings: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := sc.c.http.Do(req)
	if err != nil {
		return wrapIfUnreachable(err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return readRegistryError(resp)
	}
	return nil
}
