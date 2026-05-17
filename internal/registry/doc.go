// Package registry is Tether's registry-resolution layer for the S5
// platform-reshape cutover. It resolves launch inputs (runtime bindings,
// agents, MCP servers, execution templates / boot specs) through the
// go-agent-launch directory registry instead of a bespoke catalog walk.
//
// This package builds ONLY the resolution layer. It deliberately does not
// wire itself into any launch front-end, the daemon, or internal/launch —
// that is a later, gated step. The future launch resolver is the intended
// caller of the query helpers here.
//
// Locked design constraints honored by this package:
//
//   - D1 — local-first. Resolution works fully offline with zero network.
//     The registrar is a go-agent-launch FileBackedRegistrar over the
//     local catalog root (default ~/.tether/catalog/), wrapped in a
//     DegradingRegistrar so a registry-down condition degrades to a
//     last-known-good cache rather than hard-failing a launch.
//
//   - D2 — handles, not content. The registry stores resolver handles /
//     file pointers (RegistrationRecord = meta + RegistrationSource), not
//     inlined content bodies. The query helpers resolve a handle, then
//     read the catalog file the handle points at to materialize a typed
//     value for the caller.
//
// Catalog-shape note. Tether's live ~/.tether/catalog/ files are
// Tether-native YAML (config.Provider, config.Agent, config.MCPServerEntry,
// config.Launch shapes), NOT go-agent-launch RegistryContract documents.
// go-agent-launch's DecodeContract expects full contract documents with a
// meta: block and therefore cannot decode the live catalog files. The
// query helpers in this package consequently hand-map Tether-native YAML
// into the go-agent-launch return types. See resolve.go for detail; this
// is the principal go-agent-launch API friction recorded for S5.
package registry
