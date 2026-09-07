package a2aadapter

// card.go — builds the a2a.AgentCard advertised at each binding's
// well-known discovery path. Capabilities are declared, not aspirational:
// Streaming and PushNotifications are always false here, and
// a2asrv.WithCapabilityChecks (adapter.go) wires the SAME struct into the
// request handler so the SDK itself enforces it (T10 acceptance #3).

import (
	"github.com/a2aproject/a2a-go/v2/a2a"
)

// buildAgentCard constructs the public AgentCard for binding, advertised
// at rpcURL (the JSON-RPC endpoint this card's SupportedInterfaces entry
// points at).
func buildAgentCard(binding AgentBinding, rpcURL string) *a2a.AgentCard {
	skillID := binding.ID
	skillName := binding.DisplayName
	if skillName == "" {
		skillName = binding.ID
	}
	description := binding.Description
	if description == "" {
		description = "Tether-relayed agent, reachable via the A2A protocol."
	}

	skillDescription := "Accepts messages relayed into Tether's canonical messaging service."
	if binding.TaskMode {
		skillDescription = "Accepts delegated work; a Tether-side consumer decides the outcome via an explicit, authorized transition."
	}

	return &a2a.AgentCard{
		Name:        skillName,
		Description: description,
		Version:     string(a2a.Version),
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(rpcURL, a2a.TransportProtocolJSONRPC),
		},
		Capabilities: a2a.AgentCapabilities{
			Streaming:         false,
			PushNotifications: false,
			ExtendedAgentCard: false,
		},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{
			{
				ID:          skillID,
				Name:        skillName,
				Description: skillDescription,
				Tags:        []string{"tether", "relay"},
			},
		},
	}
}
