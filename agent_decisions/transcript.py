"""Read Claude Code and Codex transcripts into the parts the gates use."""

from __future__ import annotations

import hashlib
import json
import re
import shlex
from dataclasses import dataclass
from pathlib import Path
from typing import BinaryIO, Iterable, Iterator

from .policy import QUESTION_TOOLS

# Codex records its own context as user messages; these prefixes mark them.
_CODEX_INJECTED = (
    "# AGENTS.md instructions",
    "<environment_context>",
    "<user_instructions>",
    "The following is the Codex agent history",
    "<codex_delegation>",
    "<subagent_notification>",
    "<external_codex_apps_open_page>",
)
# Claude Code echoes shell and slash-command output as origin-less user records.
_CLAUDE_ECHOES = ("<bash-stdout>", "<bash-stderr>", "<local-command-stdout>", "<local-command-stderr>")


def is_codex_hook_feedback(text: str) -> bool:
    """Codex emits hook feedback as user-role records without a human origin."""
    return text.lstrip().startswith(("<hook_prompt ", "<hook_prompt>"))


@dataclass(frozen=True)
class HostIdentity:
    thread_id: str
    session_id: str
    parent_thread_id: str | None
    agent_id: str | None
    cwd: str


@dataclass(frozen=True)
class ToolCall:
    id: str
    name: str
    input: str
    output: str = ""
    execution: dict | None = None  # native host fields remain observations, not verification


@dataclass(frozen=True)
class Transcript:
    user_messages: tuple[str, ...]  # the user's own words, oldest first
    tool_calls: tuple[ToolCall, ...]  # oldest first
    turn_calls: tuple[ToolCall, ...]  # calls after the latest user (or parent) message
    last_assistant_text: str = ""
    brief: str | None = None  # a subagent's task, written by its parent agent
    user_keys: tuple[str, ...] = ()  # host record identities, including timestamps when present
    question_reply_keys: tuple[str, ...] = ()  # host replies matched to earlier completed questions
    identity: HostIdentity | None = None

    @property
    def turn_key(self) -> str:
        latest = self.user_messages[-1] if self.user_messages else (self.brief or "")
        seed = f"{len(self.user_messages)}\0{latest}".encode()
        return hashlib.sha256(seed).hexdigest()[:16]


def _question_reply_items(text: str) -> list[dict]:
    prefix, suffix = "<send_user_message_question_reply>", "</send_user_message_question_reply>"
    text = text.strip()
    if not text.startswith(prefix) or not text.endswith(suffix):
        return []
    try:
        replies = json.loads(text[len(prefix):-len(suffix)])
    except ValueError:
        return []
    if not isinstance(replies, list) or not replies:
        return []
    if not all(isinstance(reply, dict) and all(isinstance(reply.get(key), str)
                                             for key in ("questionItemId", "question", "answer"))
               for reply in replies):
        return []
    return replies


def question_reply_words(text: str) -> tuple[str, ...]:
    """Only decoded answers are human words; the envelope also repeats the agent's question."""
    return tuple(reply["answer"] for reply in _question_reply_items(text))


def _codex_question_reply(text: str, calls: tuple[ToolCall, ...]) -> bool:
    replies = _question_reply_items(text)
    if not replies:
        return False
    questions = {call.id: call for call in calls if call.name in QUESTION_TOOLS and call.output}
    for reply in replies:
        try:
            identity = json.loads(reply["questionItemId"])
            if not isinstance(identity, list) or len(identity) != 3:
                return False
            name, call_id, index = identity
            if not isinstance(call_id, str) or type(index) is not int or index < 0:
                return False
            call = questions.get(call_id)
            if call is None or name != call.name:
                return False
            request = json.loads(call.input)
            question = request["questions"][index]
            if reply["question"] != question.get("title", question.get("question")):
                return False
        except (ValueError, TypeError, KeyError, IndexError, AttributeError):
            return False
    return True


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
    lexer = shlex.shlex(command, posix=True)
    lexer.whitespace_split = True
    lexer.commenters = ""
    try:
        first = next(lexer, None)
        if first is None or (Path(first).name != "ts-decide" and first not in ("printf", "cat")):
            return False  # ordinary commands do not need their script bodies tokenized
        tokens = [first, *lexer]
    except ValueError:
        return False
    return (Path(first).name == "ts-decide" or
                             len(tokens) == 3 and tokens[0] == "printf" and tokens[1] == "%s\\n"
                             and tokens[2].startswith(("ts-decide verdict:", "ts-decide: decision unverified (")) or
                             len(tokens) == 2 and tokens[0] == "cat" and Path(tokens[1]).parent.name == "verdicts")


def load(path: str | Path, host: str) -> Transcript:
    with open(path, "rb") as stream:
        # Both Codex passes use the same append-only snapshot.
        stream.seek(0, 2)
        end = stream.tell()
        stream.seek(0)
        if host == "codex":
            identity = _codex_identity(_records(stream, end))
            stream.seek(0)
            parsed = _codex_records(_records(stream, end), identity)
        else:
            parsed = {"claude": _claude}[host](_records(stream, end))
    if not parsed.user_messages and not parsed.tool_calls and parsed.brief is None and parsed.identity is None:
        raise OSError("no usable transcript records")
    return parsed


def _records(stream: BinaryIO, end: int) -> Iterator[dict]:
    while stream.tell() < end:
        line = stream.readline(end - stream.tell())
        if not line:
            break
        try:
            record = json.loads(line.decode("utf-8", errors="replace"))
        except ValueError:
            continue  # a line still being written, or a damaged one
        if isinstance(record, dict):
            yield record


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
        self.question_reply_keys: list[str] = []
        self.user_occurrences: dict[str, int] = {}
        self.calls: list[tuple[str, str, str]] = []
        self.call_ids: set[str] = set()
        self.outputs: dict[str, str] = {}
        self.executions: dict[str, dict] = {}
        self.turn_start = 0
        self.last_text = ""
        self.brief: str | None = None

    def user(self, text: str, record: dict, *, question_reply: bool = False) -> None:
        self.users.append(text)
        key = hashlib.sha256(json.dumps(record, sort_keys=True).encode()).hexdigest()
        count = self.user_occurrences.get(key, 0) + 1
        self.user_occurrences[key] = count
        identity = f"{key}:{count}"
        self.user_keys.append(identity)
        if question_reply:
            self.question_reply_keys.append(identity)
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
        calls = tuple(ToolCall(i, n, inp, self.outputs.get(i, ""), self.executions.get(i)) for i, n, inp in self.calls)
        return Transcript(tuple(self.users), calls, calls[self.turn_start:], self.last_text,
                          self.brief, tuple(self.user_keys), tuple(self.question_reply_keys))


def _claude(records: Iterable[dict]) -> Transcript:
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
    return _codex_records(records, _codex_identity(records))


def _codex_records(records: Iterable[dict], identity: HostIdentity | None) -> Transcript:
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
                b.executions[call_id] = {key: item[key] for key in ("cwd", "status", "exit_code", "process_id")
                                         if key in item}
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
            if p.get("role") == "user" and text.lstrip().startswith("<send_user_message_question_reply"):
                questions = tuple(ToolCall(i, n, inp, b.outputs.get(i, "")) for i, n, inp in b.calls
                                  if n in QUESTION_TOOLS)
                if (p.get("origin") or {}).get("kind", "human") == "human" and _codex_question_reply(text, questions):
                    b.user(text, r, question_reply=True)
            elif p.get("role") == "user" and text.lstrip().startswith("<codex_delegation>"):
                b.parent(text)
            elif p.get("role") == "user" and text and identity is not None and identity.agent_id is not None \
                    and (p.get("origin") or {}).get("kind") != "human" \
                    and not text.lstrip().startswith(_CODEX_INJECTED) and not is_codex_hook_feedback(text):
                b.parent(text)
            elif p.get("role") == "user" and text and not text.lstrip().startswith(_CODEX_INJECTED) \
                    and not is_codex_hook_feedback(text) \
                    and (p.get("origin") or {}).get("kind", "human") == "human":
                questions = tuple(ToolCall(i, n, inp, b.outputs.get(i, "")) for i, n, inp in b.calls
                                  if n in QUESTION_TOOLS)
                matched_reply = _codex_question_reply(text, questions)
                if text.lstrip().startswith("<send_user_message_question_reply") and not matched_reply:
                    continue
                b.user(text, r, question_reply=matched_reply)
            elif p.get("role") == "assistant" and text:
                b.last_text = text
        elif kind == "function_call":
            b.call(p.get("call_id", ""), p.get("name", ""), p.get("arguments", ""))
        elif kind == "custom_tool_call":
            if p.get("name") != "exec":
                b.call(p.get("call_id", ""), p.get("name", ""), p.get("input", ""))
        elif kind in ("function_call_output", "custom_tool_call_output"):
            b.output(p.get("call_id", ""), p.get("output", ""))
    built = b.build()
    return Transcript(built.user_messages, built.tool_calls, built.turn_calls,
                      built.last_assistant_text, built.brief, built.user_keys,
                      built.question_reply_keys, identity)


def _codex_identity(records: Iterable[dict]) -> HostIdentity | None:
    metadata = (record.get("payload") for record in records if record.get("type") == "session_meta")
    first = next(metadata, None)
    if not isinstance(first, dict) or "source" not in first:
        return None  # older host transcripts do not contain native identity
    if any(item != first for item in metadata):
        raise OSError("conflicting host identity records")
    thread_id = first.get("id")
    session_id = first.get("session_id", thread_id)
    cwd = first.get("cwd")
    valid_id = lambda value: isinstance(value, str) and re.fullmatch(r"[A-Za-z0-9_-]{1,128}", value)
    if not valid_id(thread_id) or not valid_id(session_id) or not isinstance(cwd, str) or not cwd:
        raise OSError("invalid host identity")
    source = first["source"]
    if isinstance(source, str):
        return HostIdentity(thread_id, session_id, None, None, cwd)
    subagent = source.get("subagent") if isinstance(source, dict) else None
    spawn = subagent.get("thread_spawn") if isinstance(subagent, dict) else None
    if not isinstance(spawn, dict):
        raise OSError("unsupported host agent identity")
    parent = spawn.get("parent_thread_id")
    agent = spawn.get("agent_path")
    depth = spawn.get("depth")
    if not valid_id(parent) or parent == thread_id or not isinstance(agent, str):
        raise OSError("invalid host ancestry")
    parts = agent.split("/")
    if len(parts) < 3 or parts[:2] != ["", "root"] or type(depth) is not int \
            or depth != len(parts) - 2 or depth > 32 \
            or any(not re.fullmatch(r"[A-Za-z0-9_-]{1,80}", part) for part in parts[2:]):
        raise OSError("invalid canonical agent path")
    return HostIdentity(thread_id, session_id, parent, agent, cwd)
