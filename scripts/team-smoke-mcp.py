#!/usr/bin/env python3
"""MCP stdio driver for scripts/team-smoke.sh: a launched session's own
credential (TETHER_TOKEN in env) lists tether_team_* through `tether mcp`,
forms a team, messages workers and dissolves it.

usage: team-smoke-mcp.py <tether-bin> <catalog> <team-form.json> [mcp flags...]
Prints the run id on the line "RUN <id>" and "TOOLS <n>"; exits non-zero on failure.
"""
import json, os, time, subprocess, sys
K = os.environ.get("SMOKE_KEY", "a")

binary, catalog, form_path, *flags = sys.argv[1:]
proc = subprocess.Popen([binary, "--catalog", catalog, "mcp", *flags], stdin=subprocess.PIPE,
                        stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True)
n = 0

def rpc(method, params=None, notify=False):
    global n
    msg = {"jsonrpc": "2.0", "method": method}
    if params is not None:
        msg["params"] = params
    if not notify:
        n += 1
        msg["id"] = n
    proc.stdin.write(json.dumps(msg) + "\n")
    proc.stdin.flush()
    if notify:
        return None
    while True:
        line = proc.stdout.readline()
        if not line:
            sys.exit("FAIL: mcp server closed")
        resp = json.loads(line)
        if resp.get("id") == n:
            return resp

def call(name, args):
    resp = rpc("tools/call", {"name": name, "arguments": args})
    if "error" in resp or resp["result"].get("isError"):
        sys.exit("FAIL: %s -> %s" % (name, json.dumps(resp)[:600]))
    r = resp["result"]
    return r.get("structuredContent") or json.loads(r["content"][0]["text"])

rpc("initialize", {"protocolVersion": "2024-11-05", "capabilities": {}, "clientInfo": {"name": "smoke", "version": "0"}})
rpc("notifications/initialized", notify=True)
names = []
cursor = None
while True:
    resp = rpc("tools/list", {"cursor": cursor} if cursor else {})
    names += [t["name"] for t in resp["result"]["tools"]]
    cursor = resp["result"].get("nextCursor")
    if not cursor:
        break
team = [x for x in names if x.startswith("tether_team_")]
print("TOOLS", len(names), "team:", len(team))
if "tether_team_form" not in team:
    sys.exit("FAIL: tether_team_form not listed (%d tools)" % len(names))
form = json.load(open(form_path))
run = call("tether_team_form", {"key": "smoke-session-form-" + K, "request": form})["run"]["id"]
print("RUN", run)
if os.environ.get("SMOKE_HOOK"):
    os.system(os.environ["SMOKE_HOOK"])
# A bad caller-supplied filter is a request error, never internal_error.
for extra in ({"history": "bogus"}, {"kind": "bogus"}):
    req = {"run_id": run, "address": "@workers", "body": "probe"}
    req.update(extra)
    r = rpc("tools/call", {"name": "tether_team_address", "arguments": {"key": "bad-%s-%s" % (K, list(extra)[0]), "request": req}})
    code = (r["result"].get("structuredContent") or {}).get("code")
    if code != "invalid_request":
        sys.exit("FAIL: %s -> %s, want invalid_request" % (extra, code))
got = os.environ.get("SMOKE_GOT")  # file the fake worker appends every turn it receives
for verb in ("address", "assign", "delegate"):
    text = "session %s %s" % (verb, K)
    call("tether_team_" + verb, {"key": "smoke-session-%s-%s" % (verb, K), "request": {"run_id": run, "address": "@workers", "body": text}})
    if got:
        for _ in range(60):
            if os.path.exists(got) and text in open(got).read():
                break
            time.sleep(0.5)
        else:
            sys.exit("FAIL: worker never received %r" % text)
        print("DELIVERED", verb)
call("tether_team_dissolve", {"key": "smoke-session-dissolve-" + K, "request": {"run_id": run}})
print("OK")
proc.stdin.close()
proc.terminate()
