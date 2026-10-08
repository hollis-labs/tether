package mcpgateway

import (
	"encoding/json"
	"fmt"
)

const ToolsEnv = "TETHER_MCP_TOOLS"

// ParseToolAllowlist preserves explicit [] as deny-all. null and malformed
// selectors fail closed rather than becoming an inherited grant.
func ParseToolAllowlist(value string) (*Profile, error) {
	var names []string
	if err := json.Unmarshal([]byte(value), &names); err != nil || names == nil {
		return nil, fmt.Errorf("%s must be a JSON array of tool names", ToolsEnv)
	}
	p := &Profile{Tools: ToolRules{Allow: names}}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return p, nil
}
