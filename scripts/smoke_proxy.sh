#!/usr/bin/env bash
# smoke_proxy.sh — end-to-end smoke test for `mux mcp --proxy`
#
# Drives the MCP stdio server with raw JSON-RPC frames and checks:
#   1. initialize handshake succeeds
#   2. tools/list includes both native mux_* tools and proxied hadron_* tools
#   3. tools/call for hadron_health returns a non-error result
#
# Usage:
#   ./scripts/smoke_proxy.sh [path/to/mux] [catalog-dir]
#
# Exit codes:
#   0 — all checks passed
#   1 — a check failed

set -euo pipefail

MUX="${1:-./mux}"
CATALOG="${2:-${HOME}/.agent-mux/catalog}"

# JSON-RPC frame: Content-Length header + blank line + body
frame() {
    local body="$1"
    printf "Content-Length: %d\r\n\r\n%s" "${#body}" "${body}"
}

INIT='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke-test","version":"0.1"}}}'
INITIALIZED='{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}'
LIST_TOOLS='{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'
CALL_HEALTH='{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"hadron_health","arguments":{}}}'

# Build the input sequence for the MCP server.
INPUT="$(frame "$INIT")$(frame "$INITIALIZED")$(frame "$LIST_TOOLS")$(frame "$CALL_HEALTH")"

echo "==> Starting mux mcp --proxy with catalog: $CATALOG"
RESPONSE=$(echo "$INPUT" | timeout 30 "$MUX" mcp --proxy --catalog "$CATALOG" 2>/tmp/smoke_stderr.txt || true)

if [[ -s /tmp/smoke_stderr.txt ]]; then
    echo "--- stderr ---"
    cat /tmp/smoke_stderr.txt
    echo "---"
fi

echo "==> Raw response bytes: ${#RESPONSE}"

# ── Check 1: initialize response ──────────────────────────────────────────────
if echo "$RESPONSE" | grep -q '"id":1'; then
    echo "PASS: initialize response received"
else
    echo "FAIL: no initialize response (id:1) found"
    echo "$RESPONSE" | head -5
    exit 1
fi

# ── Check 2: tools/list contains both mux_* and hadron_* ──────────────────────
if echo "$RESPONSE" | grep -q '"mux_health"'; then
    echo "PASS: native tool mux_health present in tools/list"
else
    echo "FAIL: mux_health not found in tools/list"
    exit 1
fi

if echo "$RESPONSE" | grep -qE '"hadron_[a-z]'; then
    HADRON_COUNT=$(echo "$RESPONSE" | grep -oE '"hadron_[a-z_]+"' | sort -u | wc -l | tr -d ' ')
    echo "PASS: proxied hadron_* tools present ($HADRON_COUNT unique names)"
else
    echo "FAIL: no hadron_* tools found in tools/list"
    exit 1
fi

if echo "$RESPONSE" | grep -q '"mux_catalog_list_mcp_servers"'; then
    echo "PASS: mux_catalog_list_mcp_servers introspection tool present"
else
    echo "FAIL: mux_catalog_list_mcp_servers not found"
    exit 1
fi

# ── Check 3: hadron_health call returns non-error result ──────────────────────
# The hadron_health response is the third result (id:3).
# It should NOT contain "isError":true in its result block.
HEALTH_RESP=$(echo "$RESPONSE" | grep -o '"id":3.*' | head -1 || true)
if [[ -z "$HEALTH_RESP" ]]; then
    echo "FAIL: no response for hadron_health call (id:3)"
    exit 1
fi

if echo "$HEALTH_RESP" | grep -q '"isError":true'; then
    echo "FAIL: hadron_health returned isError:true"
    echo "  $HEALTH_RESP" | head -c 300
    exit 1
else
    echo "PASS: hadron_health call returned a non-error result"
fi

echo ""
echo "==> All smoke test checks passed"
