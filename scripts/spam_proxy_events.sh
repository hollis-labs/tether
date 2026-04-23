#!/usr/bin/env bash
# scripts/spam_proxy_events.sh — Fake proxy-event spammer for widget dev
#
# Hammers POST /proxy/events with realistic tool-call data so the
# ToolCallFeedScreen widget has something to render without needing live MCP
# connections. Events are randomly timed — sometimes steady, sometimes bursty.
#
# Usage:
#   ./scripts/spam_proxy_events.sh [options]
#
# Options:
#   --addr   <host:port>   Daemon address            (default: localhost:8080)
#   --rate   <float>       Avg events/sec in steady  (default: 3)
#   --burst  <int>         Max events per burst       (default: 8)
#   --burst-chance <float> Probability [0-1] of burst (default: 0.15)
#   --error-rate  <float>  Fraction of failed calls  (default: 0.08)
#   --sessions <int>       Number of fake session IDs (default: 3)
#   --count  <int>         Total events to emit (0=∞) (default: 0)
#   --dry-run              Print JSON; don't POST
#   --quiet                Suppress per-event output
#   -h, --help             Show this help
#
# Requirements:
#   curl, bash ≥ 3.2 (macOS default is fine), awk (for float math)
#
# Examples:
#   # gentle trickle
#   ./scripts/spam_proxy_events.sh --rate 1
#
#   # high-frequency storm
#   ./scripts/spam_proxy_events.sh --rate 20 --burst 30 --burst-chance 0.4
#
#   # reproduce a mostly-error feed
#   ./scripts/spam_proxy_events.sh --error-rate 0.5 --quiet
#
#   # emit exactly 50 events then stop
#   ./scripts/spam_proxy_events.sh --count 50
# ---------------------------------------------------------------------------

set -euo pipefail

# ── Defaults ────────────────────────────────────────────────────────────────
ADDR="localhost:8080"
RATE=3            # avg events/sec during steady periods
BURST=8           # max events in a single burst
BURST_CHANCE=0.15 # probability that a given "interval" is a burst
ERROR_RATE=0.08   # fraction of events that are failures
NUM_SESSIONS=3    # number of distinct fake session UUIDs in play
COUNT=0           # 0 = run forever
DRY_RUN=false
QUIET=false

# ── Argument parsing ─────────────────────────────────────────────────────────
while [[ $# -gt 0 ]]; do
  case "$1" in
    --addr)         ADDR="$2";          shift 2 ;;
    --rate)         RATE="$2";          shift 2 ;;
    --burst)        BURST="$2";         shift 2 ;;
    --burst-chance) BURST_CHANCE="$2";  shift 2 ;;
    --error-rate)   ERROR_RATE="$2";    shift 2 ;;
    --sessions)     NUM_SESSIONS="$2";  shift 2 ;;
    --count)        COUNT="$2";         shift 2 ;;
    --dry-run)      DRY_RUN=true;       shift ;;
    --quiet)        QUIET=true;         shift ;;
    -h|--help)
      sed -n '2,/^# -\{3\}/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) echo "Unknown option: $1" >&2; exit 1 ;;
  esac
done

BASE_URL="http://${ADDR}"

# ── Realistic data pools ─────────────────────────────────────────────────────

# Servers and their associated tools (parallel arrays — same index = same server)
SERVERS=(
  hadron hadron hadron hadron hadron
  clockwork clockwork clockwork clockwork clockwork
  vanta vanta vanta
  mux mux mux mux mux mux mux mux mux mux
)
TOOLS=(
  hadron_run_enqueue hadron_blueprints_list hadron_runs_list hadron_health hadron_runs
  clockwork_task_create clockwork_task_list clockwork_sprint_create clockwork_sprint_list clockwork_task_update
  vanta_recall vanta_memory_recall vanta_capture
  mux_session_list mux_session_create mux_session_get mux_catalog_list_mcp_servers mux_events_tool_calls mux_discover mux_call mux_message_send mux_message_inbox mux_health
)

# A handful of args-schema fingerprints — 8 hex chars
SCHEMA_FPS=(
  "a1b2c3d4" "deadbeef" "cafebabe" "f00dface"
  "0badcafe" "1337beef" "c0ffee42" "abcdef01"
  "99887766" "55443322"
)

# Plausible error messages per server
HADRON_ERRS=("blueprint not found" "run timed out" "worker unavailable" "invalid blueprint YAML")
CLOCKWORK_ERRS=("sprint not found" "task already closed" "rate limit exceeded" "invalid priority value")
VANTA_ERRS=("namespace not found" "recall timeout" "conduit unreachable")
MUX_ERRS=("session not found" "upstream error" "tool call rejected: no matching server" "invalid_request: missing required param")

# Typical durations per server (ms)
HADRON_DUR_MIN=80;   HADRON_DUR_MAX=4200
CLOCKWORK_DUR_MIN=12; CLOCKWORK_DUR_MAX=380
VANTA_DUR_MIN=20;   VANTA_DUR_MAX=900
MUX_DUR_MIN=3;      MUX_DUR_MAX=120

# ── Session ID pool ──────────────────────────────────────────────────────────
# Pre-generate NUM_SESSIONS random UUIDs (v4 shape, bash-native)
SESSION_IDS=()
for ((i=0; i<NUM_SESSIONS; i++)); do
  # Generate a UUID-v4-shaped string using /dev/urandom if available,
  # else fall back to a $RANDOM-based approximation.
  if [[ -r /dev/urandom ]]; then
    uuid=$(od -x -N 16 /dev/urandom | head -1 | awk '{
      printf "%s%s-%s-%s-%s-%s%s%s\n", $2,$3,$4,$5,$6,$7,$8,$9}')
  else
    uuid=$(printf '%04x%04x-%04x-%04x-%04x-%04x%04x%04x' \
      $RANDOM $RANDOM $RANDOM $RANDOM $RANDOM $RANDOM $RANDOM $RANDOM)
  fi
  SESSION_IDS+=("$uuid")
done

# ── Helper: random float in [0,1) via awk ────────────────────────────────────
# We pass a seed derived from RANDOM (bash built-in, refreshes every call) XOR'd
# with nanoseconds so rapid back-to-back calls don't collide.
rand_float() {
  local seed=$(( RANDOM * 32768 + RANDOM ))
  awk -v seed="$seed" 'BEGIN{srand(seed); printf "%.6f\n", rand()}'
}

# ── Helper: random int in [min, max] via awk ─────────────────────────────────
rand_int() {
  local min=$1 max=$2
  local seed=$(( RANDOM * 32768 + RANDOM ))
  awk -v lo="$min" -v hi="$max" -v seed="$seed" \
    'BEGIN{srand(seed); printf "%d\n", lo + int(rand()*(hi-lo+1))}'
}

# ── Helper: pick random element from array ───────────────────────────────────
pick_random() {
  local arr=("$@")
  local n=${#arr[@]}
  local idx
  idx=$(rand_int 0 $((n - 1)))
  echo "${arr[$idx]}"
}

# ── Helper: float comparison (returns 0=true if a < b) ───────────────────────
float_lt() {
  awk -v a="$1" -v b="$2" 'BEGIN{exit !(a < b)}'
}

# ── Helper: compute sleep delay from --rate ──────────────────────────────────
# Returns a jittered delay so we don't emit at a perfectly mechanical cadence.
# Drawn from an exponential-ish distribution centred on 1/RATE.
jittered_delay() {
  local seed=$(( RANDOM * 32768 + RANDOM ))
  awk -v rate="$RATE" -v seed="$seed" 'BEGIN{
    srand(seed)
    u = rand(); if (u == 0) u = 0.0001
    d = -log(u) / rate
    if (d > 10) d = 10
    printf "%.3f\n", d
  }'
}

# ── Helper: build the JSON payload for one event ─────────────────────────────
build_event() {
  local idx
  idx=$(rand_int 0 $((${#SERVERS[@]} - 1)))
  local server="${SERVERS[$idx]}"
  local tool="${TOOLS[$idx]}"
  local session_id
  session_id=$(pick_random "${SESSION_IDS[@]}")
  local schema_fp
  schema_fp=$(pick_random "${SCHEMA_FPS[@]}")

  # Duration based on server
  local dur
  case "$server" in
    hadron)    dur=$(rand_int $HADRON_DUR_MIN $HADRON_DUR_MAX) ;;
    clockwork) dur=$(rand_int $CLOCKWORK_DUR_MIN $CLOCKWORK_DUR_MAX) ;;
    vanta)     dur=$(rand_int $VANTA_DUR_MIN $VANTA_DUR_MAX) ;;
    *)         dur=$(rand_int $MUX_DUR_MIN $MUX_DUR_MAX) ;;
  esac

  # Determine ok/error
  local ok="true"
  local error_field=""
  local r
  r=$(rand_float)
  if float_lt "$r" "$ERROR_RATE"; then
    ok="false"
    local err_msg
    case "$server" in
      hadron)    err_msg=$(pick_random "${HADRON_ERRS[@]}") ;;
      clockwork) err_msg=$(pick_random "${CLOCKWORK_ERRS[@]}") ;;
      vanta)     err_msg=$(pick_random "${VANTA_ERRS[@]}") ;;
      *)         err_msg=$(pick_random "${MUX_ERRS[@]}") ;;
    esac
    # Escape for JSON (backslash and double-quote)
    err_msg="${err_msg//\\/\\\\}"
    err_msg="${err_msg//\"/\\\"}"
    error_field=", \"error\": \"${err_msg}\""
  fi

  local ts
  ts=$(date -u +"%Y-%m-%dT%H:%M:%S.000Z" 2>/dev/null || date -u +"%Y-%m-%dT%H:%M:%SZ")

  printf '{"session_id":"%s","server":"%s","tool_name":"%s","args_schema_fp":"%s","duration_ms":%d,"ok":%s,"timestamp":"%s"%s}' \
    "$session_id" "$server" "$tool" "$schema_fp" "$dur" "$ok" "$ts" "$error_field"
}

# ── Helper: POST one event ────────────────────────────────────────────────────
post_event() {
  local payload="$1"

  if $DRY_RUN; then
    echo "[dry-run] $payload"
    return 0
  fi

  local http_code
  http_code=$(curl -s -o /dev/null -w "%{http_code}" \
    -X POST "${BASE_URL}/proxy/events" \
    -H "Content-Type: application/json" \
    -d "$payload" \
    --max-time 3 2>/dev/null) || http_code="ERR"

  if ! $QUIET; then
    local tool server ok_flag
    tool=$(echo "$payload"   | grep -o '"tool_name":"[^"]*"' | cut -d'"' -f4)
    server=$(echo "$payload" | grep -o '"server":"[^"]*"'    | cut -d'"' -f4)
    ok_flag=$(echo "$payload"| grep -o '"ok":[a-z]*'         | cut -d: -f2)
    local icon="✓"
    [[ "$ok_flag" == "false" ]] && icon="✗"
    echo "[${http_code}] ${icon} ${server:-native}  ${tool}  (${payload##*duration_ms\":} ms)"
  fi
}

# ── Banner ────────────────────────────────────────────────────────────────────
if ! $QUIET; then
  echo "╔══════════════════════════════════════════════════════════╗"
  echo "║           agent-mux  proxy-event spammer                ║"
  echo "╚══════════════════════════════════════════════════════════╝"
  echo "  target:       ${BASE_URL}/proxy/events"
  echo "  rate:         ~${RATE} events/sec (jittered)"
  echo "  burst:        up to ${BURST} events, ${BURST_CHANCE} chance/interval"
  echo "  error rate:   ${ERROR_RATE}"
  echo "  sessions:     ${NUM_SESSIONS}  (${SESSION_IDS[*]})"
  echo "  total limit:  $( [[ $COUNT -eq 0 ]] && echo '∞' || echo "$COUNT" )"
  echo "  dry-run:      ${DRY_RUN}"
  echo ""
  echo "  Ctrl-C to stop."
  echo ""
fi

# ── Main loop ────────────────────────────────────────────────────────────────
emitted=0

while true; do
  # Check --count limit
  if [[ $COUNT -gt 0 && $emitted -ge $COUNT ]]; then
    $QUIET || echo ""
    $QUIET || echo "Done — emitted ${emitted} events."
    break
  fi

  # Decide: burst or steady single event?
  local_r=$(rand_float)
  if float_lt "$local_r" "$BURST_CHANCE"; then
    # ── BURST MODE ──────────────────────────────────────────────────────────
    n=$(rand_int 2 "$BURST")
    # Honour --count cap
    if [[ $COUNT -gt 0 ]]; then
      remaining=$((COUNT - emitted))
      [[ $n -gt $remaining ]] && n=$remaining
    fi
    $QUIET || echo "  ↯ burst ×${n}"
    for ((b=0; b<n; b++)); do
      payload=$(build_event)
      post_event "$payload"
      emitted=$((emitted + 1))
      # Tiny intra-burst gap so the widget can actually see individual rows
      sleep 0.04
    done
    # After a burst, pause a bit longer than normal before resuming
    _seed=$(( RANDOM * 32768 + RANDOM ))
    pause=$(awk -v rate="$RATE" -v seed="$_seed" 'BEGIN{srand(seed); printf "%.3f\n", 1.5/rate + rand()*0.8}')
    sleep "$pause"
  else
    # ── STEADY SINGLE EVENT ─────────────────────────────────────────────────
    payload=$(build_event)
    post_event "$payload"
    emitted=$((emitted + 1))
    delay=$(jittered_delay)
    sleep "$delay"
  fi
done
