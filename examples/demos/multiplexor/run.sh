#!/usr/bin/env bash
# examples/demos/multiplexor/run.sh
#
# Demonstrates the Tether multiplexor pattern:
#   1 primary session + 2 sibling sessions in a session group,
#   communicating via broker request/reply envelopes.
#
# Prerequisites:
#   - mux daemon running: `mux daemon start`
#   - mux binary on PATH: `go install ./cmd/mux`
#   - curl available
#
# Usage:
#   bash examples/demos/multiplexor/run.sh [--catalog <path>]

set -euo pipefail

CATALOG="${1:-}"
if [[ "$CATALOG" == "--catalog" ]]; then
    CATALOG="$2"
fi
if [[ -z "$CATALOG" ]]; then
    SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
    CATALOG="$(cd "$SCRIPT_DIR/../../catalog" && pwd)"
fi

# Resolve daemon address from catalog.
DAEMON_ADDR=$(mux --catalog "$CATALOG" daemon status 2>&1 | grep listener | awk '{print $2}' || echo "")
if [[ -z "$DAEMON_ADDR" ]]; then
    echo "ERROR: daemon not running. Start it with: mux --catalog $CATALOG daemon start" >&2
    exit 1
fi

# curl helper for UDS or TCP.
daemon_curl() {
    local method="$1" path="$2" body="${3:-}"
    if [[ "$DAEMON_ADDR" == unix:* ]]; then
        local sock="${DAEMON_ADDR#unix:}"
        if [[ -n "$body" ]]; then
            curl -sf --unix-socket "$sock" -X "$method" \
                -H "Content-Type: application/json" -d "$body" "http://unix$path"
        else
            curl -sf --unix-socket "$sock" -X "$method" "http://unix$path"
        fi
    else
        local base="http://${DAEMON_ADDR#tcp:}"
        if [[ -n "$body" ]]; then
            curl -sf -X "$method" -H "Content-Type: application/json" \
                -d "$body" "$base$path"
        else
            curl -sf -X "$method" "$base$path"
        fi
    fi
}

echo "=== Tether Multiplexor Demo ==="
echo "Catalog: $CATALOG"
echo ""

# ── Step 1: Create a session group ──────────────────────────────────────────
echo "1. Creating session group..."
GROUP=$(daemon_curl POST /session-groups '{"name":"multiplexor-demo","workflow_id":"demo-wf-1"}')
GROUP_ID=$(echo "$GROUP" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "   Group ID: $GROUP_ID"
echo ""

# ── Step 2: Create three sessions (primary + 2 siblings) ────────────────────
echo "2. Creating sessions..."
create_session() {
    local launch_id="$1"
    local sess
    sess=$(daemon_curl POST /sessions "{\"launch\":\"$launch_id\"}")
    local sess_id
    sess_id=$(echo "$sess" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
    echo "$sess_id"
}

PRIMARY=$(create_session "api-stub-launch")
echo "   Primary:  $PRIMARY"
SIBLING1=$(create_session "api-stub-launch")
echo "   Sibling1: $SIBLING1"
SIBLING2=$(create_session "api-stub-launch")
echo "   Sibling2: $SIBLING2"
echo ""

# ── Step 3: Add all sessions to the group ───────────────────────────────────
echo "3. Adding sessions to group..."
for SID in "$PRIMARY" "$SIBLING1" "$SIBLING2"; do
    daemon_curl POST "/session-groups/$GROUP_ID/members" "{\"session_id\":\"$SID\"}" > /dev/null
    echo "   Added $SID"
done
echo ""

# ── Step 4: Launch all sessions ─────────────────────────────────────────────
echo "4. Launching sessions..."
for SID in "$PRIMARY" "$SIBLING1" "$SIBLING2"; do
    daemon_curl POST "/sessions/$SID/launch" '{}' > /dev/null
    echo "   Launched $SID"
done
sleep 0.3
echo ""

# ── Step 5: Primary posts a request to sibling1 ─────────────────────────────
echo "5. Primary sends request to sibling1 (async mode)..."
REQUEST=$(daemon_curl POST /broker/requests \
    "{\"sender\":\"$PRIMARY\",\"recipient\":\"$SIBLING1\",\"workflow_id\":\"demo-wf-1\",\"payload\":\"{\\\"task\\\":\\\"analyze-codebase\\\"}\"}")
CORR_ID=$(echo "$REQUEST" | python3 -c "import sys,json; print(json.load(sys.stdin)['correlation_id'])")
REQ_ID=$(echo "$REQUEST" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "   Request ID:      $REQ_ID"
echo "   Correlation ID:  $CORR_ID"
echo ""

# ── Step 6: Sibling1 posts a response ───────────────────────────────────────
echo "6. Sibling1 replies with response..."
RESPONSE=$(daemon_curl POST /broker/envelopes \
    "{\"sender\":\"$SIBLING1\",\"recipient\":\"$PRIMARY\",\"message_type\":\"response\",\"correlation_id\":\"$CORR_ID\",\"workflow_id\":\"demo-wf-1\",\"payload\":\"{\\\"result\\\":\\\"analysis-complete\\\"}\"}")
RESP_ID=$(echo "$RESPONSE" | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
echo "   Response ID: $RESP_ID"
echo ""

# ── Step 7: Primary posts a notice to sibling2 ──────────────────────────────
echo "7. Primary sends notice to sibling2..."
daemon_curl POST /broker/envelopes \
    "{\"sender\":\"$PRIMARY\",\"recipient\":\"$SIBLING2\",\"message_type\":\"notice\",\"workflow_id\":\"demo-wf-1\",\"payload\":\"{\\\"event\\\":\\\"work-complete\\\"}\"}" > /dev/null
echo "   Notice sent"
echo ""

# ── Step 8: Verify group membership ─────────────────────────────────────────
echo "8. Verifying group sessions..."
MEMBERS=$(daemon_curl GET "/session-groups/$GROUP_ID/sessions")
COUNT=$(echo "$MEMBERS" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['sessions']))")
echo "   Sessions in group: $COUNT (expected 3)"
echo ""

# ── Step 9: Query correlation thread ────────────────────────────────────────
echo "9. Querying correlation thread (workflow_id + correlation_id)..."
THREAD=$(daemon_curl GET "/broker/envelopes?workflow_id=demo-wf-1&correlation_id=$CORR_ID")
THREAD_COUNT=$(echo "$THREAD" | python3 -c "import sys,json; print(len(json.load(sys.stdin)['envelopes']))")
echo "   Envelopes in thread: $THREAD_COUNT (expected 2: request + response)"
echo ""

# ── Step 10: Stop all sessions ──────────────────────────────────────────────
echo "10. Stopping sessions..."
for SID in "$PRIMARY" "$SIBLING1" "$SIBLING2"; do
    daemon_curl POST "/sessions/$SID/stop" '{}' > /dev/null 2>&1 || true
    echo "    Stopped $SID"
done
echo ""

echo "=== Demo complete ==="
echo ""
echo "Summary:"
echo "  Group ID:    $GROUP_ID"
echo "  Request ID:  $REQ_ID (correlation: $CORR_ID)"
echo "  Response ID: $RESP_ID"
echo ""
echo "Thread viewable at:"
echo "  GET /broker/envelopes?workflow_id=demo-wf-1&correlation_id=$CORR_ID"
