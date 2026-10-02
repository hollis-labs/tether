package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hollis-labs/tether/internal/mcpgateway"
)

const KeyMCPDiscoveryMode = "mcp.discovery_mode"

type MCPSettings struct {
	DiscoveryMode *string `json:"discovery_mode,omitempty"`
}

func (s *Service) GetMCP(ctx context.Context) (MCPSettings, error) {
	setting, err := s.GetSetting(ctx, ScopeGlobal, "", KeyMCPDiscoveryMode)
	if errors.Is(err, ErrNotFound) {
		return MCPSettings{}, nil
	}
	if err != nil {
		return MCPSettings{}, err
	}
	var mode string
	if err := json.Unmarshal([]byte(setting.ValueJSON), &mode); err != nil {
		return MCPSettings{}, fmt.Errorf("stored mcp.discovery_mode: %w", err)
	}
	if err := mcpgateway.ValidateMode(mode); err != nil {
		return MCPSettings{}, err
	}
	return MCPSettings{&mode}, nil
}

// SetMCP clears the fallback when mode is omitted; an explicitly empty one is invalid.
func (s *Service) SetMCP(ctx context.Context, in MCPSettings) error {
	if in.DiscoveryMode == nil {
		return s.DeleteSetting(ctx, ScopeGlobal, "", KeyMCPDiscoveryMode)
	}
	if err := mcpgateway.ValidateMode(*in.DiscoveryMode); err != nil {
		return err
	}
	raw, err := json.Marshal(*in.DiscoveryMode)
	if err != nil {
		return err
	}
	return s.SetSetting(ctx, Setting{Scope: ScopeGlobal, Key: KeyMCPDiscoveryMode, ValueJSON: string(raw)})
}
