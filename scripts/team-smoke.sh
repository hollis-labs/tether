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
for l in tether-claude tether-claude-worktree agent-mux-codex-app-server agent-mux-codex-app-server-worktree; do
  mode=hybrid; [[ $l == *worktree ]] && mode=worktree
  printf 'id: %s\nproject: tether\nagent: general\nprovider: fake\nworkspace: {mode: %s}\nprompt: {include_project_boot: false, include_agent_boot: false}\n' "$l" "$mode" >"$CAT/launches/$l.yaml"
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
LS="$("${T[@]}" team ls "$RUN" 2>&1)" || die "ls: $LS"
echo "ls: $LS"
DIS="$("${T[@]}" team stop "$RUN" --all --key smoke-stop 2>&1)" || die "stop: $DIS"
echo "stop: $DIS"
for _ in $(seq 40); do
  [[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] && break; sleep 0.5
done
[[ -z "$("${T[@]}" sessions list 2>/dev/null | awk 'NR>1 && $2=="running"')" ]] || die "sessions still running after stop"

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
