#!/usr/bin/env python3
"""Throwaway probe: record every hook payload; rewrite or deny two marker commands."""
import json
import re
import sys
import time
from pathlib import Path

out = Path(__file__).resolve().parent / "out"
out.mkdir(exist_ok=True)
event = sys.argv[1]
payload = json.load(sys.stdin)
path = payload.get("transcript_path")
text = Path(path).read_text(errors="replace") if path and Path(path).exists() else ""
snapshot = {
    "argv": sys.argv[1:],
    "payload": payload,
    "tool_use_id_in_transcript_at_hook": bool(payload.get("tool_use_id")) and payload["tool_use_id"] in text,
    "ids_in_transcript_at_hook": sorted(set(re.findall(r'"id":"(toolu_\w+|call_\w+)"', text))),
}
(out / f"{time.time_ns()}-{event}.json").write_text(json.dumps(snapshot, indent=1))

tool_input = payload.get("tool_input") if isinstance(payload.get("tool_input"), dict) else {}
command = tool_input.get("command", tool_input.get("cmd"))
if isinstance(command, list):
    command = command[-1]
if command == "echo probe-rewrite":
    key = "command" if "command" in tool_input else "cmd"
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "allow",
        "updatedInput": {**tool_input, key: "echo rewritten"}}}))
elif command == "echo probe-deny":
    print(json.dumps({"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "deny",
        "permissionDecisionReason": "probe deny reason 7f3a"}}))
