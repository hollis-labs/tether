- Bump `libs/mcp/plugin-mcp` to v0.6.0 (CW-20261024-0130). The request-path sanitizer no longer deletes the
  closing `</svg>` or `</html>` of a long document: a close tag that balances an earlier opener is kept at any
  distance, while stray close tags are still stripped. Affects every tool called through the gateway or the MCP
  adapter (a Tangent SVG image stored 108 of 114 bytes before). Also moves `core/util` to v0.6.0 and adds
  `core/panics` as an indirect dependency; the module now requires Go 1.26.10.
