- Let headless agy agents call Tether's MCP tools (CW-20261024-0111). With
  `TETHER_AGY_MCP_ALLOWLIST=1` (off by default) each antigravity launch gets a private
  HOME: a symlink farm of the real home plus a private `.gemini` whose generated
  `settings.json` allows `mcp(tether_tether/*)`, so agy no longer auto-denies every MCP
  call. The real `~/.gemini` is never written; settings merge and revoked rules drop at
  the next launch. Verified live on macOS and Linux with agy 1.4.7. The private home is
  per session; stable native-state parity with other providers is CW-20261024-0127. See
  `docs/agy-mcp-allowlist.md`.
