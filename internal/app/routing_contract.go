package app

import "github.com/hollis-labs/tether/internal/app/routingcap"

type RuntimeRoutingCapabilities = routingcap.RuntimeRoutingCapabilities
type RoutingCapabilitiesResponse = routingcap.RoutingCapabilitiesResponse

var ErrRoutingSessionNotFound = routingcap.ErrSessionNotFound
