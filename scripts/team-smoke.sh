#!/usr/bin/env bash
# scripts/team-smoke.sh — form, message and dissolve a first team against a
# THROWAWAY daemon (temp catalog, state, socket). Never touches ~/.tether.
#
# Usage: scripts/team-smoke.sh [--keep]     (prints PASS/FAIL, exit 0/1)
#
# The catalog uses a fake claude-shaped provider (a shell loop), so no credentials are
# needed. Pins come from examples/teams/definitions via `tether team pin`.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KEEP=0; [[ "${1:-}" == "--keep" ]] && KEEP=1
export TMPDIR=/tmp
# An agent/operator shell may carry a live-daemon credential: never send it to
# the throwaway daemon, which must authenticate us by its own operator.token.
for v in $(compgen -e | grep '^TETHER_'); do unset "$v"; done
# bwrap cannot sandbox on hosts that forbid user namespaces; the sandbox is not
# under test here.
export TETHER_SANDBOX_PROTECT=0
export TETHER_TEAMS_LOOP_INTERVAL=500ms
ROOT="$(mktemp -d /tmp/team-smoke.XXXXXX)"
CAT="$ROOT/catalog"; SOCK="$ROOT/run/tetherd.sock"; BIN="$ROOT/tether"
LOG="$ROOT/daemon.log"; DPID=""
cleanup() {
  [[ -n "$DPID" ]] && kill "$DPID" 2>/dev/null && wait "$DPID" 2>/dev/null
  [[ $KEEP == 1 ]] && echo "kept $ROOT" || rm -rf "$ROOT"
}
trap cleanup EXIT
die() { echo "FAIL: $*"; echo "--- team ls"; "$BIN" --catalog "$CAT" team ls 2>&1 | head -c 3000; echo; echo "--- sessions"; "$BIN" --catalog "$CAT" sessions list 2>&1 | head; echo "--- daemon log tail ($LOG)"; tail -25 "$LOG" 2>/dev/null; KEEP=1; exit 1; }

(cd "$REPO" && go build -o "$BIN" ./cmd/tether) || die "build"
mkdir -p "$ROOT/run" "$ROOT/workspaces" "$ROOT/repo" "$CAT"/{projects,agents,providers,launches,team-definitions}
git -C "$ROOT/repo" init -q && git -C "$ROOT/repo" -c user.name=s -c user.email=s@s commit -q --allow-empty -m init

cat >"$CAT/global.yaml" <<YAML
version: 0.1.0
catalog:
  roots: {projects: projects, agents: agents, providers: providers, launches: launches}
  defaults: {workspace_root: $ROOT/workspaces, state_db: $ROOT/state/tether.db, temp_root: $ROOT/tmp}
daemon: {listen_addr: "unix:$SOCK", pid_file: "$ROOT/run/tetherd.pid", shutdown_timeout: 5s}
identity:
  mode: observe
  points: {teams: enforce}
teams: {enabled: true}
YAML
cat >"$CAT/projects/tether.yaml" <<YAML
id: tether
name: Tether
repo_root: $ROOT/repo
workspace: {default_mode: hybrid, session_root: $ROOT/workspaces/tether}
YAML
printf 'id: general\nname: General\nroles: [general]\n' >"$CAT/agents/general.yaml"
# Stand-in for the claude CLI in streaming-stdio mode: ignores its flags, logs
# every turn it receives to $ROOT/got.log and answers each with a result event.
cat >"$ROOT/fake-claude" <<SH
#!/bin/sh
echo '{"type":"system","subtype":"init","session_id":"fake"}'
# A launched session holds its own credential in TETHER_TOKEN; keep it (0600,
# never printed) so the session phase can act as that session.
( umask 077; printf '%s' "\$TETHER_TOKEN" >"$ROOT/tok.\$\$" )
while read -r line; do
  printf '%s\\n' "\$line" >>"$ROOT/got.log"
  # A turn asking for smoke-reply is answered by mail to the coordinator address
  # and own address the delivery preamble announced, using this session's credential.
  case "\$line" in *smoke-reply*)
    coord=\$(printf '%s' "\$line" | sed -n 's/.*coordinator \(msg:[^ ]*\)\. Reply.*/\1/p')
    me=\$(printf '%s' "\$line" | sed -n 's/.*you are @[a-z0-9_-]* (\(msg:[^)]*\)).*/\1/p')
    if [ -n "\$coord" ] && [ -n "\$me" ]; then
      "$BIN" --catalog "$CAT" messages notify --from "\$me" --to "\$coord" --kind response "smoke-reply-ack from worker" >>"$ROOT/reply.log" 2>&1
    else echo "no coordinator in turn" >>"$ROOT/reply.log"; fi ;;
  esac
  echo '{"type":"result","subtype":"success","result":"ok","session_id":"fake"}'
done
SH
chmod +x "$ROOT/fake-claude"
cat >"$CAT/providers/fake.yaml" <<YAML
id: fake
type: cli
provider: claude
runtime_kind: streaming-stdio
command: $ROOT/fake-claude
args: []
bootstrap: {mode: streaming-stdio, prompt_prefix: ""}
env: {mode: merge, passthrough: [HOME, PATH]}
YAML
# Stand-in for `codex app-server` (JSON-RPC over stdio): answers the handshake,
# thread/start and turn/start, logs every turn's text to $ROOT/codex-got.log and
# completes it. Like the real thing it emits nothing until it is asked to.
cat >"$ROOT/fake-codex" <<SH
#!/usr/bin/env python3
import json, sys
def out(o): sys.stdout.write(json.dumps(o) + "\\n"); sys.stdout.flush()
n = 0
for line in sys.stdin:
    try: m = json.loads(line)
    except ValueError: continue
    meth, rid, p = m.get("method"), m.get("id"), m.get("params") or {}
    if rid is None: continue
    if meth == "initialize":
        out({"id": rid, "result": {"userAgent": "fake-codex"}})
    elif meth == "thread/start":
        out({"id": rid, "result": {"thread": {"id": "fake-thread"}}})
        out({"method": "thread/started", "params": {"thread": {"id": "fake-thread"}}})
    elif meth == "turn/start":
        n += 1; tid = "turn-%d" % n
        text = " ".join(i.get("text", "") for i in p.get("input", []))
        with open("$ROOT/codex-got.log", "a") as f: f.write(text.replace("\\n", " ") + "\\n")
        turn = {"id": tid, "status": "inProgress", "items": [], "error": None}
        out({"id": rid, "result": {"turn": turn}})
        out({"method": "turn/started", "params": {"threadId": "fake-thread", "turn": turn}})
        turn = dict(turn, status="completed")
        out({"method": "turn/completed", "params": {"threadId": "fake-thread", "turn": turn}})
    else:
        out({"id": rid, "result": {}})
SH
chmod +x "$ROOT/fake-codex"
cat >"$CAT/providers/codex-app-server.yaml" <<YAML
id: codex-app-server
type: cli
command: $ROOT/fake-codex
args: []
bootstrap: {mode: jsonrpc-stdio, prompt_prefix: ""}
env: {mode: merge, passthrough: [HOME, PATH]}
YAML
# Stand-in for agy (subprocess-per-turn, --output-format stream-json): one
# process per turn, prompt in -p=<text>. Logs the prompt to $ROOT/agy-got.log,
# keeps one conversation id, and answers a smoke-reply turn by mail using the
# session's own credential, like fake-claude.
cat >"$ROOT/fake-agy" <<SH
#!/bin/sh
conv=agy-conv-1; prompt=""
while [ \$# -gt 0 ]; do
  case "\$1" in --conversation) conv=\$2; shift ;; -p=*) prompt=\${1#-p=} ;; esac
  shift
done
printf '%s\\n' "\$prompt" | tr '\\n' ' ' >>"$ROOT/agy-got.log"; echo >>"$ROOT/agy-got.log"
case "\$prompt" in *smoke-reply*)
  coord=\$(printf '%s' "\$prompt" | sed -n 's/.*coordinator \(msg:[^ ]*\)\. Reply.*/\1/p')
  me=\$(printf '%s' "\$prompt" | sed -n 's/.*you are @[a-z0-9_-]* (\(msg:[^)]*\)).*/\1/p')
  if [ -n "\$coord" ] && [ -n "\$me" ]; then
    "$BIN" --catalog "$CAT" messages notify --from "\$me" --to "\$coord" --kind response "smoke-reply-ack from agy" >>"$ROOT/agy-reply.log" 2>&1
  else echo "no coordinator in turn" >>"$ROOT/agy-reply.log"; fi ;;
esac
printf '{"event":"init","conversation_id":"%s","init":{"cwd":"/w","tools":[],"permission_mode":"request-review"}}\\n' "\$conv"
printf '{"event":"step_update","step_update":{"conversation_id":"%s","step_index":1,"state":"DONE","step_type":"agent_response","text_delta":"agy ok","usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":2}}}\\n' "\$conv"
printf '{"event":"result","result":{"conversation_id":"%s","status":"SUCCESS","response":"agy ok","num_turns":1}}\\n' "\$conv"
SH
chmod +x "$ROOT/fake-agy"
cat >"$CAT/providers/antigravity.yaml" <<YAML
id: antigravity
type: cli
provider: antigravity
runtime_kind: subprocess
command: $ROOT/fake-agy
args: []
bootstrap: {mode: prepend, prompt_prefix: ""}
env: {mode: merge, passthrough: [HOME, PATH, TETHER_TOKEN]}
YAML
for l in tether-claude tether-claude-worktree agent-mux-codex-app-server agent-mux-codex-app-server-worktree tether-agy; do
  mode=hybrid; [[ $l == *worktree ]] && mode=worktree
  prov=fake; [[ $l == agent-mux-codex-* ]] && prov=codex-app-server; [[ $l == tether-agy ]] && prov=antigravity
  printf 'id: %s\nproject: tether\nagent: general\nprovider: %s\nworkspace: {mode: %s}\nprompt: {include_project_boot: false, include_agent_boot: false}\n' "$l" "$prov" "$mode" >"$CAT/launches/$l.yaml"
done
cp "$REPO"/examples/teams/definitions/*.md "$CAT/team-definitions/"
cp "$REPO/examples/teams/teams-policy.yaml" "$CAT/teams-policy.yaml"
echo 'session_formation: true' >>"$CAT/teams-policy.yaml"

T=("$BIN" --catalog "$CAT")
# pins in the policy must match what `team pin` computes for the shipped files
for f in "$CAT"/team-definitions/*.md; do
  out="$("${T[@]}" team pin "$f")" || die "team pin $f"
  d="$(sed -n 's/.*"digest":"\([^"]*\)".*/\1/p' <<<"$out")"
  grep -q "$d" "$CAT/teams-policy.yaml" || die "policy digest drift for $f ($d)"
done

"$BIN" --catalog "$CAT" daemon run >"$LOG" 2>&1 &
DPID=$!
for _ in $(seq 100); do [[ -S "$SOCK" ]] && "${T[@]}" team ls >/dev/null 2>&1 && break; sleep 0.1; done
kill -0 "$DPID" 2>/dev/null || die "daemon exited"

FORM="$("${T[@]}" team form --key smoke-form --request-file "$REPO/examples/teams/team-form.json" 2>&1)" || die "form: $FORM"
echo "form: $FORM"
RUN="$(sed -n 's/.*"run":{"id":"\([^"]*\)".*/\1/p' <<<"$FORM")"
[[ -n "$RUN" ]] || die "no run_id in form result"
MSG="$("${T[@]}" team msg "$RUN" @workers 'hello from the smoke' --key smoke-msg 2>&1)" || die "msg: $MSG"
echo "msg: $MSG"
# The message must reach the worker's session (delivered by the team loop).
SEEN=0
for _ in $(seq 40); do
  grep -qs 'hello from the smoke' "$ROOT/got.log" && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "message never reached a worker session"
RV="$("${T[@]}" team msg "$RUN" @reviewer 'hello reviewer' --key smoke-msg-rev 2>&1)" || die "msg @reviewer: $RV"
echo "msg @reviewer: $RV"
# The Codex reviewer (app-server, never prompted) must be woken into a turn too.
SEEN=0
for _ in $(seq 60); do
  grep -qs 'hello reviewer' "$ROOT/codex-got.log" && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "message never started a turn on the Codex reviewer"
LS="$("${T[@]}" team ls "$RUN" 2>&1)" || die "ls: $LS"
echo "ls: $LS"
DIS="$("${T[@]}" team stop "$RUN" --all --key smoke-stop 2>&1)" || die "stop: $DIS"
echo "stop: $DIS"
for _ in $(seq 40); do
  [[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] && break; sleep 0.5
done
[[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] || die "sessions still running after stop"

# --- Antigravity as a team member: agy is the reviewer slot. A team delivery must
# start an agy turn; a smoke-reply turn must come back as mail to the coordinator.
AF="$("${T[@]}" team form --key smoke-form-agy --request-file "$REPO/examples/teams/team-form-agy.json" 2>&1)" || die "agy form: $AF"
echo "agy form: $AF"
ARUN="$(sed -n 's/.*"run":{"id":"\([^"]*\)".*/\1/p' <<<"$AF")"
[[ -n "$ARUN" ]] || die "no run_id in agy form result"
AM="$("${T[@]}" team msg "$ARUN" @reviewer 'hello agy smoke-reply' --key smoke-msg-agy 2>&1)" || die "msg agy @reviewer: $AM"
echo "msg agy @reviewer: $AM"
SEEN=0
for _ in $(seq 80); do
  grep -qs 'hello agy smoke-reply' "$ROOT/agy-got.log" && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "message never started a turn on the agy reviewer"
SEEN=0
for _ in $(seq 60); do
  [[ -s "$ROOT/agy-reply.log" ]] && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "agy reviewer never replied (agy-reply.log empty)"
grep -q 'no coordinator' "$ROOT/agy-reply.log" && die "agy turn carried no coordinator/own address: $(cat "$ROOT/agy-reply.log")"
echo "agy reply: $(head -c 300 "$ROOT/agy-reply.log" | tr '\n' ' ')"
"${T[@]}" team stop "$ARUN" --all --key smoke-stop-agy >/dev/null 2>&1 || die "agy stop"
for _ in $(seq 40); do
  [[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] && break; sleep 0.5
done
[[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] || die "sessions still running after agy stop"

# --- Antigravity outside a team: `sessions turn` on a launched agy session must
# run an agy turn and publish session.turn_output with the reply.
AL="$("${T[@]}" launch --launch tether-agy 2>&1)" || die "agy launch: $AL"
ASID="$(grep -oE '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}' <<<"$AL" | head -1)"
[[ -n "$ASID" ]] || ASID="$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"{print $1; exit}')"
[[ -n "$ASID" ]] || die "agy launch gave no session id: $AL"
AT="$("${T[@]}" sessions turn "$ASID" "direct agy turn" 2>&1)" || die "agy sessions turn ($ASID; launch=$AL): $AT"
SEEN=0
for _ in $(seq 60); do
  grep -qs 'direct agy turn' "$ROOT/agy-got.log" && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "sessions turn never started an agy turn"
SEEN=0
for _ in $(seq 40); do
  "${T[@]}" events history --session-id "$ASID" --kind session.turn_output 2>&1 | grep -q 'agy ok' && SEEN=1 && break; sleep 0.5
done
[[ $SEEN == 1 ]] || die "agy turn published no session.turn_output"
echo "agy sessions turn: turn_output published"
"${T[@]}" sessions stop "$ASID" >/dev/null 2>&1

# --- session phase: a launched session, using only its own credential, sees
# tether_team_* over `tether mcp`, forms a team, messages workers, dissolves.
ls "$ROOT"/tok.* >"$ROOT/tok.before" 2>/dev/null
LAUNCH="$("${T[@]}" launch --launch tether-claude 2>&1)" || die "session launch: $LAUNCH"
TOK=""
for _ in $(seq 40); do
  for f in "$ROOT"/tok.*; do
    [[ "$f" == "$ROOT/tok.before" ]] && continue
    grep -qx "$f" "$ROOT/tok.before" && continue
    [[ -s "$f" ]] && TOK="$f" && break 2
  done
  sleep 0.5
done
[[ -n "$TOK" ]] || die "launched session never recorded a credential"
for flags in "--daemon-only" "--proxy"; do
  out="$(SMOKE_GOT="$ROOT/got.log" SMOKE_KEY="${flags#--}" TETHER_TOKEN="$(cat "$TOK")" python3 -I "$REPO/scripts/team-smoke-mcp.py" "$BIN" "$CAT" "$REPO/examples/teams/team-form.json" $flags 2>&1)" || die "session mcp ($flags): $out"
  echo "session mcp ($flags): $(tr '\n' ' ' <<<"$out")"
done
echo PASS
