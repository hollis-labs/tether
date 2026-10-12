#!/usr/bin/env python3
"""Plant the operator's configured hub MCP clients in an agent boot directory.

Copies only torque, tesseract and tether from the user Claude configuration.
The Tether entry must use SSH and --daemon-only: booting an agent must never
create a local Tether authority. Other project configuration is preserved.
"""

import argparse
import json
import os
from pathlib import Path
import re
import stat
import tempfile
import tomllib


SERVERS = ("torque", "tesseract", "tether")


def read(path):
    try:
        return path.read_text()
    except FileNotFoundError:
        return None


def publish(path, original, content):
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temporary = tempfile.mkstemp(prefix=".hub-mcp-", dir=path.parent)
    try:
        mode = stat.S_IMODE(path.stat().st_mode) if original is not None else 0o600
        os.fchmod(fd, mode)
        with os.fdopen(fd, "w") as stream:
            stream.write(content)
            stream.flush()
            os.fsync(stream.fileno())
        if read(path) != original:
            raise ValueError(f"configuration changed during wiring: {path}")
        os.replace(temporary, path)
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def table(name, entry):
    lines = [f"[mcp_servers.{name}]"]
    for key in ("command", "args", "cwd", "startup_timeout_sec", "tool_timeout_sec", "enabled"):
        if key in entry:
            lines.append(f"{key} = {json.dumps(entry[key], ensure_ascii=False)}")
    if entry.get("env"):
        lines.append(f"[mcp_servers.{name}.env]")
        lines.extend(f"{json.dumps(k)} = {json.dumps(v)}" for k, v in entry["env"].items())
    return "\n".join(lines) + "\n"


def wire(directory, kind, source):
    document = json.loads(source.read_text())
    upstreams = document.get("mcpServers", {})
    entries = {}
    for name in SERVERS:
        entry = upstreams.get(name)
        if not isinstance(entry, dict) or not entry.get("command"):
            raise ValueError(f"configure the user-level {name} hub MCP entry first")
        entries[name] = entry
    tether = entries["tether"]
    args = tether.get("args", [])
    if tether["command"] != "ssh" or "--daemon-only" not in args or "--proxy" in args:
        raise ValueError("the configured Tether hub entry must use ssh and --daemon-only")

    plans = []
    if kind in ("claude", "both"):
        path = directory / ".mcp.json"
        original = read(path)
        config = json.loads(original) if original is not None else {}
        servers = config.setdefault("mcpServers", {})
        legacy = servers.get("agentos", {})
        if legacy.get("command") == "ssh" and "agent-os" in legacy.get("args", []):
            del servers["agentos"]
        servers.update(entries)
        plans.append((path, original, json.dumps(config, indent=2) + "\n"))
        path = directory / ".claude" / "settings.json"
        original = read(path)
        settings = json.loads(original) if original is not None else {}
        enabled = settings.setdefault("enabledMcpjsonServers", [])
        if "agentos" not in servers:
            enabled[:] = [name for name in enabled if name != "agentos"]
        for name in SERVERS:
            if name not in enabled:
                enabled.append(name)
        plans.append((path, original, json.dumps(settings, indent=2) + "\n"))

    if kind in ("codex", "both"):
        path = directory / ".codex" / "config.toml"
        original = read(path)
        content = original or ""
        config = tomllib.loads(content)
        servers = config.get("mcp_servers", {})
        remove = set(SERVERS)
        legacy = servers.get("agentos", {})
        if legacy.get("command") == "ssh" and "agent-os" in legacy.get("args", []):
            remove.add("agentos")
        # Remove only the tables this operation replaces, retaining other
        # tables, comments and options rather than rewriting the entire TOML.
        sections = re.split(r"(?m)(?=^\s*\[)", content)
        retained = []
        for section in sections:
            header = re.match(r'\s*\[mcp_servers\.(?:"([^"]+)"|([A-Za-z0-9_-]+))(?:\.|\])', section)
            if not header or (header.group(1) or header.group(2)) not in remove:
                retained.append(section)
        content = "".join(retained).rstrip() + "\n\n"
        content += "\n".join(table(name, entry) for name, entry in entries.items())
        user_path = Path.home() / ".codex" / "config.toml"
        user_config = tomllib.loads(read(user_path) or "")
        if "mux" not in servers and "mux" in user_config.get("mcp_servers", {}):
            content += "\n[mcp_servers.mux]\nenabled = false\n"
        tomllib.loads(content)
        plans.append((path, original, content))

    # Validate every input and generated config before publishing any of them.
    for path, original, content in plans:
        publish(path, original, content)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    parser.add_argument("kind", nargs="?", choices=("claude", "codex", "both"), default="both")
    parser.add_argument("--source", type=Path, default=Path.home() / ".claude.json")
    args = parser.parse_args()
    try:
        wire(args.directory.resolve(), args.kind, args.source)
    except (OSError, ValueError, TypeError) as error:
        parser.exit(1, f"hub MCP wiring refused: {error}\n")


if __name__ == "__main__":
    main()
