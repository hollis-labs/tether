// Package llm holds Tether's normalized AI gateway substrate.
//
// The package intentionally starts small:
//   - request/response types shared across providers
//   - a middleware contract for policy enforcement and observability
//
// Concrete provider adapters, routing policy, secret resolution, and daemon
// API surfaces will layer on top in follow-on slices.
package llm
