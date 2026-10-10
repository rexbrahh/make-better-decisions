"""Hook entry point: one payload in, one host-specific response out. Never blocks on its own failure."""

from __future__ import annotations

import argparse
import json
import os
import shlex
import sys
import time
import uuid
from dataclasses import replace
from pathlib import Path

from . import evidence, gates, jev, ledger, policy, transcript


def handle(event: str, payload: dict, host: str, *, post=jev.post) -> dict | None:
    payload = {**payload, "tool_use_id": payload.get("tool_use_id") or f"local-{uuid.uuid4().hex}"}
    if event == "session_start":
        ledger.prune()
        root = Path(__file__).resolve().parent.parent
        return {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext":
            "Use the deciding skill before choosing an approach or asking the user to decide. "
            "Jev checks each edit and shell write before execution. "
            f"Run {shlex.quote(str(root / 'bin' / 'ts-decide'))} as a separate shell call. "
            f"Read {root / 'skills' / 'deciding' / 'SKILL.md'} for the request format. "
            "Read the verdict before executing a dependent call."}}
    if event == "pre_tool_use":
        return _pre_tool_use(payload, host, post)
    if event in ("stop", "subagent_stop"):
        return _stop(payload, host, post)
    raise ValueError(f"unknown event {event!r}")


def _pre_tool_use(payload: dict, host: str, post) -> dict | None:
    name = payload.get("tool_name") or ""
    tool_input = payload.get("tool_input")
    if not isinstance(tool_input, dict):
        tool_input = {"input": tool_input}
    if name in policy.SHELL_TOOLS:
        kind, request = gates.classify(gates.command_of(tool_input))
        if kind == "decide":
            return _decide(payload, host, tool_input, request, post)
        if kind == "misuse":
            return _deny(gates.MISUSE)
    if name in policy.SHELL_TOOLS or name in policy.EDIT_TOOLS or name in policy.READ_TOOLS:
        outcome = gates.guard(tool_input)
        if outcome is not None:
            try:
                _append(payload, host, None, outcome)
            except OSError:
                pass  # the denial must not depend on the ledger being writable
            return _deny(outcome.message)
    if name in policy.SHELL_TOOLS or name in policy.EDIT_TOOLS:
        return _gate(payload, host, lambda ctx: gates.edit_gate(name, tool_input, ctx, _asker(post)))
    if name in policy.QUESTION_TOOLS:
        return _gate(payload, host, lambda ctx: gates.question_gate(tool_input, ctx, _asker(post)))
    if host == "claude" and name == "ExitPlanMode":
        return _gate(payload, host, gates.plan_gate)
    return None


def _decide(payload: dict, host: str, tool_input: dict, request: str, post) -> dict:
    try:
        ctx = _context(payload, host)
    except OSError as error:
        turn = None
        outcome = gates.Outcome("unverified", f"ts-decide: decision unverified (transcript unreadable: "
                                              f"{type(error).__name__}). Decide yourself and state your evidence.",
                                {"gate": "decide"})
    else:
        turn = ctx.transcript.turn_key
        outcome = gates.decide(request, ctx, _asker(post))
    if outcome.record is not None:
        _append(payload, host, turn, outcome)
    outcome = replace(outcome, message=evidence.redact(outcome.message, _secrets()))
    if outcome.action == "invalid":
        return _deny(outcome.message)
    path = ledger.write_verdict(payload["tool_use_id"], outcome.message)
    updated = dict(tool_input)
    for key in ("command", "cmd"):
        if key in updated:
            updated[key] = f"cat {shlex.quote(str(path))}"
    return {"hookSpecificOutput": {
        "hookEventName": "PreToolUse", "permissionDecision": "allow",
        "permissionDecisionReason": "ts-decide verdict",
        "updatedInput": updated}}


def _gate(payload: dict, host: str, run) -> dict | None:
    try:
        ctx = _context(payload, host)
    except OSError as error:
        return _note(f"agent-decisions: gate skipped, transcript unreadable ({type(error).__name__})")
    outcome = run(ctx)
    if outcome.record is not None:
        _append(payload, host, ctx.transcript.turn_key, outcome)
    if outcome.action == "deny":
        return _deny(outcome.message)
    return _note(outcome.note) if outcome.note else None


def _stop(payload: dict, host: str, post) -> dict | None:
    try:
        ctx = _context(payload, host)
    except OSError as error:
        return _note(f"agent-decisions: stop check skipped, transcript unreadable ({type(error).__name__})")
    final = payload.get("last_assistant_message")
    final = final if isinstance(final, str) else ctx.transcript.last_assistant_text
    outcome = gates.stop_gate(final, bool(payload.get("stop_hook_active")), ctx, _asker(post))
    if outcome.record is not None:
        _append(payload, host, ctx.transcript.turn_key, outcome)
    if outcome.action == "block":
        return {"decision": "block", "reason": outcome.message}
    return _note(outcome.note) if outcome.note else None


def _load_transcript(payload: dict, host: str) -> transcript.Transcript:
    main_path = payload.get("transcript_path") or ""
    own_path = payload.get("agent_transcript_path")
    agent_id = payload.get("agent_id")
    if not own_path and agent_id and main_path:
        guess = Path(main_path).with_suffix("") / "subagents" / f"agent-{agent_id}.jsonl"
        own_path = str(guess) if guess.exists() else None
    if not own_path or own_path == main_path:
        return transcript.load(main_path, host)
    own = transcript.load(own_path, host)
    return replace(own, user_messages=transcript.load(main_path, host).user_messages)


def _context(payload: dict, host: str) -> gates.Context:
    t = _load_transcript(payload, host)
    secrets = _secrets()
    session = payload.get("session_id") or "unknown"
    return gates.Context(host=host, session_id=session, agent_id=payload.get("agent_id"),
                         tool_use_id=payload["tool_use_id"], transcript=t,
                         evidence=evidence.build(t, secrets), entries=tuple(ledger.entries(session)))


def _secrets() -> tuple[str, ...]:
    try:
        return (jev.load_key(),)
    except jev.JevError:
        return ()


def _asker(post):
    def ask(state, questions):
        key = jev.load_key()
        return jev.ask(evidence.scrub(state, (key,)), evidence.scrub(questions, (key,)), key=key, post=post)
    return ask


def _append(payload: dict, host: str, turn: str | None, outcome: gates.Outcome) -> None:
    ledger.append({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "host": host,
                   "session_id": payload.get("session_id") or "unknown", "agent_id": payload.get("agent_id"),
                   "tool_use_id": payload["tool_use_id"], "turn": turn, "action": outcome.action,
                   **evidence.scrub(outcome.record, _secrets())})


def _deny(reason: str) -> dict:
    return {"hookSpecificOutput": {"hookEventName": "PreToolUse", "permissionDecision": "deny",
                                   "permissionDecisionReason": reason[:10_000]}}


def _note(text: str) -> dict:
    return {"systemMessage": text}


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="agent-decisions hook")
    parser.add_argument("event", choices=("pre_tool_use", "stop", "subagent_stop", "session_start"))
    parser.add_argument("--host", required=True, choices=("claude", "codex"))
    args = parser.parse_args(argv)
    if os.environ.get("AGENT_DECISIONS_OFF") == "1":
        return 0
    try:
        out = handle(args.event, json.load(sys.stdin), args.host)
    except Exception as error:  # fail open: an outage or a bug must never block the agent
        out = _note(f"agent-decisions: hook error ({type(error).__name__}); action allowed")
    if out:
        json.dump(out, sys.stdout)
    return 0
