#!/usr/bin/env bash
# post-deploy-check.sh — read-only post-deploy check for a deployed Tether hub.
#
# Walks the live-check checklist in docs/releases/2026-10-14-overnight.md
# (section 3) in row order and prints PASS / FAIL / SKIP / MANUAL per row.
#   PASS    the read-only part of the row was observed.
#   FAIL    the read-only part of the row was observed and is wrong.
#   SKIP    could not be checked here (no data, tool missing, wrong target).
#   MANUAL  needs a state change or a human; the exact command is printed and
#           NOT run.
# Rows G1..G9 are general hub checks that have no checklist row.
#
# Read-only by construction: only `tether version/doctor/workers/sessions
# list|tasks|peek/execution status/projects list/agents list`, GET /capacity
# and the read-only MCP tools (tools/list, catalog_*, session_peek) are used.
# Nothing is launched, stopped, planned, applied, paused or reaped.
#
# Usage:  scripts/post-deploy-check.sh
# Env:    TETHER_BIN      tether binary (default: tether)
#         TETHER_TARGET   remote hub forward, as for the CLI (GET /capacity is
#                         then SKIPped; it needs the local socket)
#         TETHER_SOCK     daemon socket (default: ~/.tether/run/tetherd.sock)
#         EXPECT_COMMIT   if set, G1 requires the version line to contain it
#         MCP_WAIT        seconds to let `tether mcp` answer (default: 4)
# Needs:  jq, curl (rows needing them are SKIPped when absent).
# Exit:   1 if any row is FAIL, otherwise 0.

set -u

TETHER_BIN=${TETHER_BIN:-tether}
TETHER_SOCK=${TETHER_SOCK:-$HOME/.tether/run/tetherd.sock}
MCP_WAIT=${MCP_WAIT:-4}

fails=0
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

say() { printf '%-6s %-4s %s\n' "$1" "$2" "$3"; }
pass() { say PASS "$1" "$2"; }
skip() { say SKIP "$1" "$2"; }
fail() { say FAIL "$1" "$2"; fails=$((fails + 1)); }
manual() { say MANUAL "$1" "$2"; printf '              run: %s\n' "$3"; }

have() { command -v "$1" >/dev/null 2>&1; }

# t runs the tether CLI with a timeout.
t() { timeout 60 "$TETHER_BIN" "$@"; }

# mcp_call TOOL ARGS_JSON [SCOPES]: one read-only tools/call through a fresh
# `tether mcp`; prints the JSON-RPC response for id 2 on stdout.
mcp_call() {
	local tool=$1 args=${2:-'{}'} scopes=${3:-}
	local init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"post-deploy-check","version":"1"}}}'
	local note='{"jsonrpc":"2.0","method":"notifications/initialized"}'
	local req
	if [ "$tool" = "tools/list" ]; then
		req='{"jsonrpc":"2.0","id":2,"method":"tools/list"}'
	else
		req=$(jq -cn --arg n "$tool" --argjson a "$args" '{jsonrpc:"2.0",id:2,method:"tools/call",params:{name:$n,arguments:$a}}')
	fi
	{
		printf '%s\n%s\n%s\n' "$init" "$note" "$req"
		sleep "$MCP_WAIT"
	} | TETHER_MCP_SCOPES="$scopes" timeout $((MCP_WAIT + 20)) "$TETHER_BIN" mcp 2>/dev/null |
		jq -c 'select(.id == 2)' 2>/dev/null | head -n 1
}

# mcp_text RESPONSE: the text content of a tools/call response.
mcp_text() { jq -r '[.result.content[]? | .text] | join("\n")' <<<"$1"; }

echo "== general =="

# G1 version / commit
if out=$(t --version 2>&1); then
	if [ -n "${EXPECT_COMMIT:-}" ] && [[ $out != *"$EXPECT_COMMIT"* ]]; then
		fail G1 "version line lacks EXPECT_COMMIT=$EXPECT_COMMIT: $out"
	else
		pass G1 "$out"
	fi
else
	fail G1 "tether --version failed: $out"
fi

# G2 doctor (exit 1 on any failed check; read-only migration check, #291)
if out=$(t doctor 2>&1); then
	pass G2 "tether doctor: no failed checks"
else
	fail G2 "tether doctor reported failures: $(tail -n 5 <<<"$out" | tr '\n' ' ')"
fi

# G3 workers connected, orphaned_hosts surfaced (#286)
if ! have jq; then
	skip G3 "jq missing"
elif out=$(t workers --json 2>&1); then
	n=$(jq '.workers | length' <<<"$out" 2>/dev/null || echo "")
	if [ -z "$n" ]; then
		fail G3 "workers --json is not the expected JSON: $out"
	elif [ "$n" -eq 0 ]; then
		skip G3 "no workers connected"
	elif jq -e '[.workers[] | has("orphaned_hosts")] | all' >/dev/null <<<"$out"; then
		pass G3 "$n worker(s); orphaned_hosts per worker: $(jq -c '[.workers[] | {id: (.id // .label), orphaned_hosts}]' <<<"$out")"
	else
		fail G3 "worker record without orphaned_hosts (worker/hub wire mismatch, #286)"
	fi
else
	fail G3 "tether workers failed: $out"
fi

# G4 sessions list answers via the daemon
sid=""
if out=$(t sessions list 2>&1); then
	sid=$(awk 'NR == 2 { print $1 }' <<<"$out")
	pass G4 "sessions list ok ($(($(wc -l <<<"$out") - 1)) session(s))"
else
	fail G4 "sessions list failed: $out"
fi

# G5 projects / agents list (catalog loaded by the daemon)
projects=""
if projects=$(t projects list 2>&1); then
	pass G5 "projects list ok"
else
	fail G5 "projects list failed: $projects"
fi

# G6 MCP tool list contains the tools added in this batch
tools_json=""
if ! have jq; then
	skip G6 "jq missing"
else
	resp=$(mcp_call tools/list)
	tools_json=$resp
	if [ -z "$resp" ]; then
		fail G6 "tether mcp gave no tools/list response"
	else
		missing=""
		for want in tether_catalog_validate tether_catalog_files tether_catalog_list_launches \
			tether_catalog_plan tether_catalog_apply tether_session_peek tether_team_form; do
			jq -e --arg w "$want" '[.result.tools[]?.name] | index($w)' >/dev/null <<<"$resp" || missing="$missing $want"
		done
		if [ -z "$missing" ]; then
			pass G6 "mcp tools/list has the new/changed tools"
		else
			fail G6 "mcp tools/list is missing:$missing"
		fi
	fi
fi

# G7 catalog validate against the live catalog (read-only, no files given)
if ! have jq; then
	skip G7 "jq missing"
elif resp=$(mcp_call tether_catalog_validate '{}') && [ -n "$resp" ]; then
	if jq -e '.result.isError == true or .error' >/dev/null <<<"$resp"; then
		fail G7 "catalog validate errored: $(mcp_text "$resp" | head -c 300)"
	else
		txt=$(mcp_text "$resp")
		errs=$(jq -r '[.. | objects | select(.severity? == "error")] | length' <<<"$txt" 2>/dev/null || echo "")
		if [ -n "$errs" ] && [ "$errs" -gt 0 ]; then
			fail G7 "live catalog validation has $errs error finding(s)"
		else
			pass G7 "live catalog validates (no error findings)"
		fi
	fi
else
	fail G7 "no response from tether_catalog_validate"
fi

# G8 catalog files listing
files_txt=""
if have jq && resp=$(mcp_call tether_catalog_files '{}') && [ -n "$resp" ] && ! jq -e '.result.isError == true or .error' >/dev/null <<<"$resp"; then
	files_txt=$(mcp_text "$resp")
	pass G8 "tether_catalog_files lists $(grep -c . <<<"$files_txt") line(s)"
elif have jq; then
	fail G8 "tether_catalog_files failed"
else
	skip G8 "jq missing"
fi

# G9 launches list (also row 9 read-only part)
launches_txt=""
if have jq && resp=$(mcp_call tether_catalog_list_launches '{}') && [ -n "$resp" ] && ! jq -e '.result.isError == true or .error' >/dev/null <<<"$resp"; then
	launches_txt=$(mcp_text "$resp")
	pass G9 "tether_catalog_list_launches ok"
elif have jq; then
	fail G9 "tether_catalog_list_launches failed"
else
	skip G9 "jq missing"
fi

echo "== checklist rows =="

manual 1 "headless worker-agent with stop_after_final_turn ends with reason final_turn" \
	"$TETHER_BIN launch --launch <worker-agent-launch> --worker <worker>   # then watch session.reaper"
manual 2 "queued routing reply during the grace window defers the stop" \
	"$TETHER_BIN sessions turn <session> '<text>' --reply-to <addr>   # inside the grace window"
manual 3 "routing reply to a non-running session settles undeliverable/session_ended after 5 attempts" \
	"sqlite3 -readonly <state_db> \"select id,state,attempts,updated_at from routing_replies order by updated_at desc limit 20\""
manual 4 "bad hub-mcp.json: worker WARN, session.launch_failed event, 'session launch failed: <cause>'" \
	"plant a bad hub-mcp.json on a worker, then: $TETHER_BIN launch --launch <id> --worker <worker>"
manual 5 "headless launch with MCP servers and permission_mode default logs a WARN" \
	"$TETHER_BIN launch --launch <headless-id>; journalctl --user -u tetherd | grep -i permission_mode"

# 6 read-only part: example launch and project present in the live catalog
if [ -z "$launches_txt" ]; then
	skip 6 "launch list unavailable"
elif grep -q 'task-worker' <<<"$launches_txt" && grep -qw tether <<<"$projects"; then
	pass 6 "launch task-worker and project tether exist in the live catalog"
else
	fail 6 "task-worker launch and/or project 'tether' missing from the live catalog (copy examples/catalog per the runbook)"
fi
manual 6 "launch task-worker with --torque-task and confirm the closing step" \
	"$TETHER_BIN launch --launch task-worker --torque-task <CW-id>"

# 7 read-only part: sessions tasks
if out=$(t sessions tasks --json 2>&1); then
	pass 7 "sessions tasks ok"
else
	fail 7 "sessions tasks failed: $out"
fi
manual 7 "launch --replace on a stopped session rebinds; on a live one returns 409 idempotency_conflict" \
	"$TETHER_BIN launch --launch <id> --idempotency-key K --replace"
manual 8 "--idempotency-key auto twice returns the same session" \
	"$TETHER_BIN launch --launch <id> --idempotency-key auto   # run twice, identical inputs"

# 9 read-only part: every launch reports enabled
if [ -z "$launches_txt" ]; then
	skip 9 "launch list unavailable"
elif [ "$(jq -r '(.launches // .) | length' <<<"$launches_txt" 2>/dev/null)" = 0 ]; then
	skip 9 "catalog has no launches"
elif jq -e '(.launches // .) | type == "array" and all(.[]; has("enabled"))' >/dev/null 2>&1 <<<"$launches_txt"; then
	pass 9 "every launch in tether_catalog_list_launches reports enabled"
else
	fail 9 "tether_catalog_list_launches output has no per-launch enabled field"
fi
manual 9 "disabled agent returns 409 agent_disabled; reload with a broken catalog changes nothing" \
	"$TETHER_BIN launch --launch <launch-of-disabled-agent>   # expect 409; and tether_catalog_reload with a broken catalog"
manual 10 "Tangent chat roster shows zz-probe only while enabled" "open the Tangent chat roster with zz-probe enabled, then disabled"
manual 11 "teams-policy.yaml apply reports restart_required" \
	"plan then apply a teams-policy.yaml change (tether_catalog_plan / $TETHER_BIN catalog approve <plan_id> / tether_catalog_apply)"
manual 12 "plan expiry: identical re-plan returns same plan_id with fresh expiry; expired plan refused" \
	"tether_catalog_plan twice with the same bundle across the TTL; $TETHER_BIN catalog show <plan_id>"

# 13 read-only part: a team-definitions file carries its pin
td=$(grep -o 'team-definitions/[A-Za-z0-9._/-]*' <<<"$files_txt" | head -n 1)
if [ -z "$td" ]; then
	skip 13 "no team-definitions file in the catalog"
elif resp=$(mcp_call tether_catalog_files "$(jq -cn --arg p "$td" '{path:$p}')") && [ -n "$resp" ] &&
	mcp_text "$resp" | grep -q 'digest'; then
	pass 13 "$td returns a pin (compare with: $TETHER_BIN team pin)"
else
	fail 13 "tether_catalog_files $td has no pin/digest"
fi

manual 14 "hub restart stops without status=1/FAILURE inside shutdown_timeout" \
	"systemctl --user restart tetherd; journalctl --user -u tetherd -n 50   # owner action"

# 15 read-only part: team tools present
if [ -z "$tools_json" ]; then
	skip 15 "tools/list unavailable"
elif jq -e '[.result.tools[]?.name] | index("tether_team_form")' >/dev/null <<<"$tools_json"; then
	pass 15 "tether_team_form is exposed"
else
	fail 15 "tether_team_form missing from tools/list (teams disabled?)"
fi
manual 15 "form a real 1 coordinator + 2 worker + reviewer team; reaper stops one past max_duration" \
	"tether_team_form (key + request) from an operator session"
manual 16 "reviewer pairing with two allowlisted runtimes, then with one" \
	"tether_team_form twice with two runtimes, then one; read ReviewerPairing in the result"

# 17 read-only part: --reply-to flag shipped
if t sessions turn --help 2>&1 | grep -q -- '--reply-to'; then
	pass 17 "sessions turn --reply-to is available"
else
	fail 17 "sessions turn has no --reply-to flag"
fi
manual 17 "message to a registered local operator session shows [reply-to: ...]" \
	"$TETHER_BIN sessions turn <id> '<text>' --reply-to <addr>   # after tether registry register + claude-hook"

# 18 sessions peek on a recent session; MCP refuses without terminal scope
if [ -z "$sid" ]; then
	skip 18 "no session to peek"
elif out=$(t sessions peek "$sid" 2>&1); then
	pass 18 "sessions peek $sid returned a digest ($(wc -l <<<"$out") line(s)); shape depends on provider"
else
	fail 18 "sessions peek $sid failed: $(head -c 300 <<<"$out")"
fi
if ! have jq; then
	skip 18 "jq missing for the MCP scope-refusal check"
elif [ -z "$sid" ]; then
	skip 18 "no session for the MCP scope-refusal check"
elif resp=$(mcp_call tether_session_peek "$(jq -cn --arg s "$sid" '{session_id:$s}')" "") && [ -n "$resp" ]; then
	if jq -e '.result.isError == true or .error' >/dev/null <<<"$resp"; then
		pass 18 "tether_session_peek refused without terminal scope"
	else
		fail 18 "tether_session_peek answered without terminal scope"
	fi
else
	fail 18 "no response from tether_session_peek"
fi

# 19 execution policy read and /capacity
if out=$(t execution status 2>&1); then
	pass 19 "execution status: $(head -n 1 <<<"$out")"
else
	fail 19 "execution status failed: $out"
fi
if [ -n "${TETHER_TARGET:-}" ]; then
	skip 19 "GET /capacity needs the local socket (TETHER_TARGET is set)"
elif ! have curl || ! have jq; then
	skip 19 "curl or jq missing for GET /capacity"
elif [ ! -S "$TETHER_SOCK" ]; then
	skip 19 "no socket at $TETHER_SOCK"
elif out=$(curl -fsS --max-time 20 --unix-socket "$TETHER_SOCK" http://unix/capacity 2>&1); then
	adm=$(jq -r '.admission // empty' <<<"$out" 2>/dev/null)
	case $adm in
	open | paused | full) pass 19 "/capacity admission=$adm" ;;
	*) fail 19 "/capacity has no valid admission field: $out" ;;
	esac
else
	fail 19 "GET /capacity failed: $out"
fi
manual 19 "pause refuses launches (409 execution_paused); caps admit exactly one of two concurrent launches" \
	"$TETHER_BIN execution pause --reason test; $TETHER_BIN launch --launch <id>; $TETHER_BIN execution resume; $TETHER_BIN execution cap ..."

# 20 retire-shim is shipped
if t sessions retire-shim --help >/dev/null 2>&1; then
	pass 20 "sessions retire-shim is available"
else
	fail 20 "sessions retire-shim missing"
fi
manual 20 "retire a session in outcome_unknown becomes orphaned (shim_retired_by_operator)" \
	"$TETHER_BIN sessions retire-shim <outcome_unknown-session-id>"

# 21 cascade stop flag is shipped
if t sessions stop --help 2>&1 | grep -q -- '--cascade'; then
	pass 21 "sessions stop --cascade is available"
else
	fail 21 "sessions stop has no --cascade flag"
fi
manual 21 "defaults.lineage_max_depth: 1 gives 409 lineage_limit_exceeded; stop --cascade stops descendants" \
	"set defaults.lineage_max_depth: 1 in global.yaml, restart, bootstrap a grandchild; $TETHER_BIN sessions stop --cascade <id>"
manual 22 "Cerberus read allowed, write refused for read-only, durable machine_authority_decision events" \
	"from a Cerberus-granted session call a read and a write tool; $TETHER_BIN events history | grep machine_authority_decision"
manual 23 "Codex projection_invalid detach: root cause unproven, nothing to check" "none (tracked in CW-20261011-0038)"
manual 24 "macOS worker GH_TOKEN fix, then git push of a throwaway branch" \
	"on the macOS worker host: find the GH_TOKEN source (launchd plist or env file), replace, restart worker, git push"
manual 25 "definitionresolve unsafe-names test passes on macOS without skip" \
	"go test ./internal/definitionresolve -run TestContentRefusesUnsafeNamesAndMembers -v"
manual 26 "boot proxy anonymity test passes on macOS" \
	"go test ./cmd/tether -run TestBootProxyEnvironmentIsExplicitlyAnonymous"
manual 27 "go test ./... on a Mac: few or no t.TempDir failures" "go test ./..."
manual 28 "operator decision on the 112-entry registry retire list" \
	"review the task comment on CW-20261013-0012; export binding_list JSON per URN first (nothing deleted)"

echo
if [ "$fails" -gt 0 ]; then
	echo "RESULT: $fails FAIL"
	exit 1
fi
echo "RESULT: no FAIL (MANUAL rows still need a human)"
