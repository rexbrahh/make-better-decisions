"""Read Claude Code and Codex transcripts into the parts the gates use."""

from __future__ import annotations

import hashlib
import json
import shlex
from dataclasses import dataclass
from pathlib import Path

# Codex records its own context as user messages; these prefixes mark them.
_CODEX_INJECTED = (
    "# AGENTS.md instructions",
    "<environment_context>",
    "<user_instructions>",
    "The following is the Codex agent history",
    "<codex_delegation>",
    "<subagent_notification>",
)
# Claude Code echoes shell and slash-command output as origin-less user records.
_CLAUDE_ECHOES = ("<bash-stdout>", "<bash-stderr>", "<local-command-stdout>", "<local-command-stderr>")


@dataclass(frozen=True)
class ToolCall:
    id: str
    name: str
    input: str
    output: str = ""


@dataclass(frozen=True)
class Transcript:
    user_messages: tuple[str, ...]  # the user's own words, oldest first
    tool_calls: tuple[ToolCall, ...]  # oldest first
    turn_calls: tuple[ToolCall, ...]  # calls after the latest user (or parent) message
    last_assistant_text: str = ""
    brief: str | None = None  # a subagent's task, written by its parent agent
    user_keys: tuple[str, ...] = ()  # host record identities, including timestamps when present

    @property
    def turn_key(self) -> str:
        latest = self.user_messages[-1] if self.user_messages else (self.brief or "")
        seed = f"{len(self.user_messages)}\0{latest}".encode()
        return hashlib.sha256(seed).hexdigest()[:16]


def is_decide(call: ToolCall) -> bool:
    """True for a ts-decide call or the hook's literal verdict transport."""
    try:
        tool_input = json.loads(call.input)
    except ValueError:
        tool_input = call.input
    command = tool_input.get("command", tool_input.get("cmd", "")) if isinstance(tool_input, dict) else tool_input
    if isinstance(command, list):
        command = command[2] if len(command) == 3 and command[1] in ("-c", "-lc") else shlex.join(command)
    if not isinstance(command, str):
        return False
    try:
        tokens = shlex.split(command)
    except ValueError:
        return False
    return bool(tokens) and (Path(tokens[0]).name == "ts-decide" or
                             len(tokens) == 3 and tokens[0] == "printf" and tokens[1] == "%s\\n"
                             and tokens[2].startswith(("ts-decide verdict:", "ts-decide: decision unverified (")) or
                             len(tokens) == 2 and tokens[0] == "cat" and Path(tokens[1]).parent.name == "verdicts")


def load(path: str | Path, host: str) -> Transcript:
    parse = {"claude": _claude, "codex": _codex}[host]
    records = []
    with open(path, encoding="utf-8", errors="replace") as f:
        for line in f:
            try:
                record = json.loads(line)
            except ValueError:
                continue  # a line still being written, or a damaged one
            if isinstance(record, dict):
                records.append(record)
    parsed = parse(records)
    if not parsed.user_messages and not parsed.tool_calls and parsed.brief is None:
        raise OSError("no usable transcript records")
    return parsed


def _text(content) -> str:
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(
            block.get("text", "") for block in content
            if isinstance(block, dict) and block.get("type") in ("text", "input_text", "output_text"))
    return ""


class _Builder:
    def __init__(self):
        self.users: list[str] = []
        self.user_keys: list[str] = []
        self.user_occurrences: dict[str, int] = {}
        self.calls: list[tuple[str, str, str]] = []
        self.call_ids: set[str] = set()
        self.outputs: dict[str, str] = {}
        self.turn_start = 0
        self.last_text = ""
        self.brief: str | None = None

    def user(self, text: str, record: dict) -> None:
        self.users.append(text)
        key = hashlib.sha256(json.dumps(record, sort_keys=True).encode()).hexdigest()
        count = self.user_occurrences.get(key, 0) + 1
        self.user_occurrences[key] = count
        self.user_keys.append(f"{key}:{count}")
        self.turn_start = len(self.calls)

    def parent(self, text: str) -> None:
        if self.brief is None:
            self.brief = text
        self.turn_start = len(self.calls)

    def call(self, call_id: str, name: str, tool_input) -> None:
        if call_id in self.call_ids:
            return  # classic response items and completed events can describe the same call
        self.call_ids.add(call_id)
        if not isinstance(tool_input, str):
            tool_input = json.dumps(tool_input, sort_keys=True, ensure_ascii=False)
        self.calls.append((call_id, name, tool_input))

    def output(self, call_id: str, output) -> None:
        if not isinstance(output, str):
            output = _text(output) or json.dumps(output, ensure_ascii=False)
        self.outputs[call_id] = output

    def build(self) -> Transcript:
        calls = tuple(ToolCall(i, n, inp, self.outputs.get(i, "")) for i, n, inp in self.calls)
        return Transcript(tuple(self.users), calls, calls[self.turn_start:], self.last_text,
                          self.brief, tuple(self.user_keys))


def _claude(records: list[dict]) -> Transcript:
    b = _Builder()
    for r in records:
        content = (r.get("message") or {}).get("content")
        if r.get("type") == "assistant" and isinstance(content, list):
            for block in content:
                if not isinstance(block, dict):
                    continue
                if block.get("type") == "tool_use":
                    b.call(block.get("id", ""), block.get("name", ""), block.get("input"))
                elif block.get("type") == "text" and block.get("text"):
                    b.last_text = block["text"]
        elif r.get("type") == "user":
            results = [x for x in content if isinstance(x, dict) and x.get("type") == "tool_result"] \
                if isinstance(content, list) else []
            for x in results:
                b.output(x.get("tool_use_id", ""), x.get("content"))
            text = _text(content)
            if results or not text or r.get("isCompactSummary") or r.get("isMeta"):
                continue
            if r.get("isSidechain"):
                b.parent(text)
            elif (r.get("origin") or {}).get("kind") == "human":
                b.user(text, r)
            elif r.get("origin") is None and not text.startswith(_CLAUDE_ECHOES):
                b.user(text, r)  # a headless `claude -p` prompt carries no origin
    return b.build()


def _codex(records: list[dict]) -> Transcript:
    b = _Builder()
    for r in records:
        if r.get("type") == "event_msg":
            p = r.get("payload") or {}
            item = p.get("item") or {}
            if p.get("type") != "item_completed":
                continue
            call_id = item.get("id", "")
            if item.get("type") == "CommandExecution":
                b.call(call_id, "Bash", {"command": item.get("command", [])})
                b.output(call_id, item.get("aggregated_output", ""))
            elif item.get("type") == "McpToolCall":
                b.call(call_id, f"mcp__{item.get('server', '')}__{item.get('tool', '')}", item.get("arguments"))
                b.output(call_id, item.get("result"))
            elif item.get("type") == "FileChange":
                b.call(call_id, "apply_patch", item.get("changes"))
                b.output(call_id, f"{item.get('stdout', '')}\n{item.get('stderr', '')}")
            continue
        if r.get("type") != "response_item":
            continue
        p = r.get("payload") or {}
        kind = p.get("type")
        if kind == "message":
            text = _text(p.get("content"))
            if p.get("role") == "user" and text and not text.lstrip().startswith(_CODEX_INJECTED) \
                    and (p.get("origin") or {}).get("kind", "human") == "human":
                b.user(text, r)
            elif p.get("role") == "assistant" and text:
                b.last_text = text
        elif kind == "function_call":
            b.call(p.get("call_id", ""), p.get("name", ""), p.get("arguments", ""))
        elif kind == "custom_tool_call":
            if p.get("name") != "exec":
                b.call(p.get("call_id", ""), p.get("name", ""), p.get("input", ""))
        elif kind in ("function_call_output", "custom_tool_call_output"):
            b.output(p.get("call_id", ""), p.get("output", ""))
    return b.build()
