package routingcap

import "errors"

var ErrSessionNotFound = errors.New("routing session not found")

// RuntimeRoutingCapabilities describes the paths available for a resolved
// runtime. Per-output confidence remains authoritative for any actual turn.
type RuntimeRoutingCapabilities struct {
	RouteSupported      bool     `json:"route_supported"`
	ReplyToSender       bool     `json:"reply_to_sender"`
	Interrupt           bool     `json:"interrupt"`
	KindsAvailable      []string `json:"kinds_available"`
	FinalTextConfidence string   `json:"final_text_confidence"`
}

// RoutingCapabilitiesResponse is shared by HTTP and MCP consumer surfaces.
// Runtime keys are registry primary ids, rather than catalog provider aliases.
type RoutingCapabilitiesResponse struct {
	RouteSupported bool                                  `json:"route_supported"`
	ReplyToSender  bool                                  `json:"reply_to_sender"`
	Interrupt      bool                                  `json:"interrupt"`
	KindsAvailable []string                              `json:"kinds_available"`
	Delivery       string                                `json:"delivery"`
	Runtimes       map[string]RuntimeRoutingCapabilities `json:"runtimes"`
	SessionID      string                                `json:"session_id,omitempty"`
}
