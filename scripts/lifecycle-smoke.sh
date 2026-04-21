#!/usr/bin/env bash
# scripts/lifecycle-smoke.sh — Session lifecycle audit diagnostic
#
# Exercises session-lifecycle edge cases against an isolated daemon instance
# and reports PASS/FAIL per edge. Each edge maps to a T-v004-s01-04 audit item.
#
# Usage:
#   ./scripts/lifecycle-smoke.sh [--catalog <path>]
#
# Requirements:
#   - 'mux' binary on PATH (run: go install ./cmd/mux)
#   - 'curl' available
#   - macOS or Linux
#
# Edge cases covered:
#   E1  Session completes (stop) while unattached → state persisted correctly
#   E2  Daemon crash (SIGKILL) with running session → restart reconciles stale state
#   E3  Three rapid attach/detach cycles → attachCount = 0 after all detach
#   E4  PTY child survives SIGKILL (informational, not a daemon bug)
#   E5  Workspace prune mechanism → no prune command exists (FAIL/defer)

set -euo pipefail

###############################################################################
# Configuration
###############################################################################

CATALOG="${1:-}"
if [[ "$CATALOG" == "--catalog" ]]; then
    CATALOG="$2"
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

if [[ -z "$CATALOG" ]]; then
    CATALOG="$REPO_ROOT/examples/catalog"
fi

if ! command -v mux &>/dev/null; then
    echo "ERROR: 'mux' not on PATH. Run: go install ./cmd/mux" >&2
    exit 1
fi

# Isolated scratch dir so smoke doesn't touch the real state DB or socket.
SMOKE_TMP="$(mktemp -d)"
SMOKE_DB="$SMOKE_TMP/smoke.db"

trap cleanup EXIT

###############################################################################
# Helpers
###############################################################################

PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0

pass() { echo "  PASS  $1"; PASS_COUNT=$((PASS_COUNT + 1)); }
fail() { echo "  FAIL  $1: $2"; FAIL_COUNT=$((FAIL_COUNT + 1)); }
skip() { echo "  SKIP  $1: $2"; SKIP_COUNT=$((SKIP_COUNT + 1)); }

# make_catalog <sock> <pid> — creates a fresh catalog dir under SMOKE_TMP
# with isolated socket, pid, and DB paths; returns the catalog dir path via echo.
make_catalog() {
    local sock="$1" pid="$2"
    local dir
    dir="$(mktemp -d "$SMOKE_TMP/cat.XXXXXX")"

    # Copy the source catalog's subdirectories (agents, projects, launches, etc.)
    # so agent/project/launch resolution still works.
    for subdir in agents projects providers launches boot; do
        if [[ -d "$CATALOG/$subdir" ]]; then
            cp -r "$CATALOG/$subdir" "$dir/$subdir"
        fi
    done

    # Write a global.yaml with our isolated paths.
    cat >"$dir/global.yaml" <<YAML
version: 0.1.0
catalog:
  roots:
    projects: projects
    agents: agents
    providers: providers
    launches: launches
  defaults:
    workspace_root: $SMOKE_TMP/workspaces
    state_db: $SMOKE_DB
    temp_root: $SMOKE_TMP/tmp
daemon:
  listen_addr: "unix:$sock"
  pid_file: "$pid"
  shutdown_timeout: "5s"
YAML

    echo "$dir"
}

# curl_uds <socket> <method> <path> [body]
curl_uds() {
    local sock="$1" method="$2" path="$3" body="${4:-}"
    if [[ -n "$body" ]]; then
        curl -sf --unix-socket "$sock" -X "$method" \
            -H "Content-Type: application/json" \
            -d "$body" "http://unix$path"
    else
        curl -sf --unix-socket "$sock" -X "$method" "http://unix$path"
    fi
}

wait_daemon_up() {
    local sock="$1" deadline=$((SECONDS + 8))
    while [[ $SECONDS -lt $deadline ]]; do
        if curl -sf --unix-socket "$sock" "http://unix/health" &>/dev/null; then
            return 0
        fi
        sleep 0.1
    done
    echo "ERROR: daemon at $sock did not start within 8s" >&2
    return 1
}

wait_session_state() {
    local sock="$1" id="$2" want="$3" deadline=$((SECONDS + 10))
    local state=""
    while [[ $SECONDS -lt $deadline ]]; do
        state=$(curl_uds "$sock" GET "/sessions/$id" \
            | python3 -c "import sys,json; print(json.load(sys.stdin)['state'])" 2>/dev/null || echo "")
        if [[ "$state" == "$want" ]]; then
            return 0
        fi
        sleep 0.2
    done
    echo "timed out waiting for state=$want (last=$state)" >&2
    return 1
}

# start_daemon <sock> <pid> <log> <catalog> — starts daemon in background,
# waits for it to be ready.
start_daemon() {
    local sock="$1" pid_file="$2" log="$3" cat_dir="$4"
    mux --catalog "$cat_dir" daemon run >"$log" 2>&1 &
    wait_daemon_up "$sock"
}

stop_daemon() {
    local pid_file="$1"
    if [[ -f "$pid_file" ]]; then
        local pid; pid=$(cat "$pid_file")
        kill -TERM "$pid" 2>/dev/null || true
        local deadline=$((SECONDS + 8))
        while [[ $SECONDS -lt $deadline ]] && kill -0 "$pid" 2>/dev/null; do
            sleep 0.1
        done
    fi
}

create_and_launch() {
    local sock="$1"
    local id
    id=$(curl_uds "$sock" POST "/sessions" '{"launch":"api-stub-launch"}' \
         | python3 -c "import sys,json; print(json.load(sys.stdin)['id'])")
    curl_uds "$sock" POST "/sessions/$id/launch" '{}' >/dev/null
    echo "$id"
}

cleanup() {
    # Kill any daemon processes started during the smoke. Use pidfiles if
    # present; fall back to pattern-match on SMOKE_TMP path in argv.
    for pf in "$SMOKE_TMP"/*.pid; do
        [[ -f "$pf" ]] || continue
        local pid; pid=$(cat "$pf")
        kill -KILL "$pid" 2>/dev/null || true
    done
    rm -rf "$SMOKE_TMP"
}

###############################################################################
# E1 — Session stops while unattached → terminal state persisted
###############################################################################

echo
echo "E1  Session completes (stop) while unattached"

E1_SOCK="$SMOKE_TMP/e1.sock"
E1_PID="$SMOKE_TMP/e1.pid"
E1_LOG="$SMOKE_TMP/e1.log"
E1_CAT="$(make_catalog "$E1_SOCK" "$E1_PID")"

start_daemon "$E1_SOCK" "$E1_PID" "$E1_LOG" "$E1_CAT"
SID1=$(create_and_launch "$E1_SOCK")
sleep 0.3

curl_uds "$E1_SOCK" POST "/sessions/$SID1/stop" '{}' >/dev/null

if wait_session_state "$E1_SOCK" "$SID1" "killed"; then
    pass "E1  session → killed after stop while unattached"
else
    fail "E1" "session not in 'killed' state after stop"
fi

stop_daemon "$E1_PID"

###############################################################################
# E2 — Daemon crash (SIGKILL) → restart reconciles stale sessions
###############################################################################

echo
echo "E2  Daemon crash (SIGKILL) with running session → restart sweeps stale state"

E2_SOCK="$SMOKE_TMP/e2.sock"
E2_PID="$SMOKE_TMP/e2.pid"
E2_LOG="$SMOKE_TMP/e2.log"
# Share the DB with the previous edge so we test against a DB with prior history,
# same as a real crash scenario.
E2_CAT="$(make_catalog "$E2_SOCK" "$E2_PID")"

start_daemon "$E2_SOCK" "$E2_PID" "$E2_LOG" "$E2_CAT"
SID2=$(create_and_launch "$E2_SOCK")
sleep 0.3

STATE_BEFORE=$(curl_uds "$E2_SOCK" GET "/sessions/$SID2" \
    | python3 -c "import sys,json; print(json.load(sys.stdin)['state'])" 2>/dev/null || echo "unknown")

if [[ "$STATE_BEFORE" != "running" ]]; then
    fail "E2-setup" "expected running before crash, got $STATE_BEFORE"
else
    # SIGKILL — no graceful shutdown, watch goroutine never completes
    DAEMON_PID=$(cat "$E2_PID")
    kill -KILL "$DAEMON_PID" 2>/dev/null || true
    sleep 0.5

    # Restart a fresh daemon against the same DB
    E2B_SOCK="$SMOKE_TMP/e2b.sock"
    E2B_PID="$SMOKE_TMP/e2b.pid"
    E2B_LOG="$SMOKE_TMP/e2b.log"
    E2B_CAT="$(make_catalog "$E2B_SOCK" "$E2B_PID")"

    start_daemon "$E2B_SOCK" "$E2B_PID" "$E2B_LOG" "$E2B_CAT"
    sleep 0.3

    STATE_AFTER=$(curl_uds "$E2B_SOCK" GET "/sessions/$SID2" \
        | python3 -c "import sys,json; print(json.load(sys.stdin)['state'])" 2>/dev/null || echo "unknown")

    if [[ "$STATE_AFTER" == "failed" ]]; then
        pass "E2  session state = failed after daemon crash+restart (SweepStaleSessions ran)"
    else
        fail "E2" "expected state=failed after restart, got $STATE_AFTER"
    fi

    stop_daemon "$E2B_PID"
fi

###############################################################################
# E3 — Rapid attach/detach cycles → attachCount accurate
###############################################################################

echo
echo "E3  Rapid attach/detach cycles → attachCount = 0 after all detach"

E3_SOCK="$SMOKE_TMP/e3.sock"
E3_PID="$SMOKE_TMP/e3.pid"
E3_LOG="$SMOKE_TMP/e3.log"
E3_CAT="$(make_catalog "$E3_SOCK" "$E3_PID")"

start_daemon "$E3_SOCK" "$E3_PID" "$E3_LOG" "$E3_CAT"
SID3=$(create_and_launch "$E3_SOCK")
sleep 0.3

# Three sequential attach cycles; each curl times out after 0.3s via read-timeout.
for i in 1 2 3; do
    curl -sf --unix-socket "$E3_SOCK" --max-time 0.3 \
        "http://unix/sessions/$SID3/attach" \
        --no-buffer >/dev/null 2>&1 || true
    sleep 0.1
done

# Give detach callbacks time to settle
sleep 0.5

COUNT=$(curl_uds "$E3_SOCK" GET "/sessions/$SID3" \
    | python3 -c "import sys,json; print(json.load(sys.stdin).get('attached_clients', -1))" 2>/dev/null || echo "-1")

if [[ "$COUNT" == "0" ]]; then
    pass "E3  attached_clients = 0 after 3 attach/detach cycles"
else
    fail "E3" "attached_clients = $COUNT after 3 cycles (want 0)"
fi

# Verify DB: all client_attachments rows for this session should have detached_at set
OPEN=$(python3 - "$SMOKE_DB" "$SID3" <<'PYEOF'
import sys, sqlite3
try:
    conn = sqlite3.connect(sys.argv[1])
    row = conn.execute(
        "SELECT count(*) FROM client_attachments WHERE session_id=? AND detached_at IS NULL",
        (sys.argv[2],)
    ).fetchone()
    print(row[0])
except Exception:
    print(-1)
PYEOF
)

if [[ "$OPEN" == "0" ]]; then
    pass "E3  all client_attachments rows have detached_at stamped"
else
    fail "E3" "$OPEN client_attachments row(s) still open (detached_at NULL) — see SPRINT-02 ATTACH PERSISTENCE backlog"
fi

stop_daemon "$E3_PID"

###############################################################################
# E4 — PTY child survives daemon SIGKILL (informational)
###############################################################################

echo
echo "E4  PTY child orphan after daemon SIGKILL (informational)"
skip "E4" "$(cat <<'MSG'
api-stub has no real child process (PID=0). Manual procedure for real PTY providers:
  1. mux --catalog <catalog> daemon start
  2. mux --catalog <catalog> sessions list   # note session ID
  3. SESS_ID=<id>; PID=\$(mux --catalog <catalog> sessions get \$SESS_ID | grep '^pid' | awk '{print \$2}')
  4. DAEMON_PID=\$(cat \$(mux --catalog <catalog> daemon status 2>&1 | grep pidfile | awk '{print \$NF}'))
  5. kill -KILL \$DAEMON_PID
  6. ps -p \$PID    # if process still alive: orphan adopted by launchd (macOS expected behavior)
  7. kill \$PID     # clean up orphan manually
  Mitigation: checkpoint-resume (v0.0.4 Sprint 4) allows re-attaching to orphans after daemon restart.
MSG
)"

###############################################################################
# E5 — Workspace prune mechanism
###############################################################################

echo
echo "E5  Workspace prune mechanism"
if mux --help 2>&1 | grep -q "workspaces"; then
    pass "E5  'mux workspaces' subcommand exists"
else
    fail "E5" "no 'mux workspaces' command — workspace dirs accumulate under workspace_root. Defer: add 'mux workspaces prune [--older-than <duration>]'."
fi

###############################################################################
# Summary
###############################################################################

echo
echo "────────────────────────────────────────────────────────────────"
echo "Results: $PASS_COUNT passed  $FAIL_COUNT failed  $SKIP_COUNT skipped"
echo "────────────────────────────────────────────────────────────────"

if [[ $FAIL_COUNT -gt 0 ]]; then
    exit 1
fi
