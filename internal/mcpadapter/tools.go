package mcpadapter

import "github.com/mark3labs/mcp-go/server"

// registerTools wires every MCP tool onto s. Tools are grouped by domain;
// each group is registered in its own file.
func (a *Adapter) registerTools(s *server.MCPServer) {
	a.mcp = s
	a.registerHealthTools(s)
	a.registerCatalogTools(s)
	a.registerAgentOpsTools(s)
	a.registerSkillTools(s)
	a.registerSessionTools(s)
	a.registerLogicalAgentTools(s)
	a.registerMessageTools(s)
	a.registerBootTools(s)
	a.registerObservationTools(s)
	a.registerAITools(s)
	a.registerRegistryTools(s)
	a.registerGroupTools(s)
}
