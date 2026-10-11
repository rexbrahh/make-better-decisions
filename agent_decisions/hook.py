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
        if not payload.get("agent_id") and payload.get("transcript_path"):
            try:
                t = _load_transcript(payload, host)
                _saved_users(payload, host, t, ledger.entries(payload.get("session_id") or "unknown"))
            except OSError:
                pass  # an empty startup transcript is normal
        root = Path(__file__).resolve().parent.parent
        return {"hookSpecificOutput": {"hookEventName": "SessionStart", "additionalContext":
            "Use the deciding skill before choosing an approach or asking the user to decide. "
            "Jev checks each edit and shell write before execution. "
            f"Run {shlex.quote(str(root / 'bin' / 'ts-decide'))} as a separate shell call. "
            f"Read {root / 'skills' / 'deciding' / 'SKILL.md'} for the request format. "
            "Read the verdict before executing a dependent call."}}
    if event == "user_prompt_submit":
        prompt = payload.get("prompt")
        if isinstance(prompt, str) and prompt and not payload.get("agent_id"):
            _append(payload, host, None, gates.Outcome("observed", record={"gate": "user_prompt", "prompt": prompt}))
        return None
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
    outcome = gates.guard(tool_input, payload.get("cwd"))
    if outcome is not None:
        try:
            _append(payload, host, None, outcome)
        except OSError:
            pass  # the denial must not depend on the ledger being writable
        return _deny(outcome.message)
    if name in policy.QUESTION_TOOLS:
        return _gate(payload, host, lambda ctx: gates.question_gate(tool_input, ctx, _asker(post)))
    if host == "claude" and name == "ExitPlanMode":
        plan = tool_input.get("plan")
        return _gate(payload, host, lambda ctx: gates.plan_gate(
            ctx, _asker(post), plan=plan if isinstance(plan, str) else None))
    if name in policy.READ_TOOLS:
        return None
    return _gate(payload, host, lambda ctx: gates.edit_gate(name, tool_input, ctx, _asker(post)))


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
        if any(c.id == ctx.tool_use_id and c.output for c in ctx.transcript.tool_calls) or any(
                e.get("gate") == "decide" and e.get("tool_use_id") == ctx.tool_use_id for e in ctx.entries):
            return _deny("ts-decide: this tool call has already completed. Run a new, separate ts-decide call.")
        outcome = gates.decide(request, ctx, _asker(post))
    outcome = replace(outcome, message=evidence.redact(outcome.message, _secrets()))
    if outcome.record is not None:
        outcome = replace(outcome, record={**outcome.record, "verdict": outcome.message})
        _append(payload, host, turn, outcome)
    if outcome.action == "invalid":
        return _deny(outcome.message)
    updated = dict(tool_input)
    for key in ("command", "cmd"):
        if key in updated:
            updated[key] = "printf '%s\\n' " + shlex.quote(outcome.message)
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
    main = transcript.load(main_path, host)
    return replace(own, user_messages=main.user_messages, user_keys=main.user_keys,
                   question_reply_keys=main.question_reply_keys)


def _context(payload: dict, host: str) -> gates.Context:
    t = _load_transcript(payload, host)
    secrets = _secrets()
    session = payload.get("session_id") or "unknown"
    entries = ledger.entries(session)
    t = replace(t, user_messages=_saved_users(payload, host, t, entries))
    bundle = evidence.build(t, secrets,
        query=json.dumps(payload.get("tool_input") or payload.get("last_assistant_message") or ""),
        cwd=payload.get("cwd") or os.getcwd())
    bundle["metadata"]["user_history_source"] = "signed_host_context"
    evidence._trim_to_budget(bundle)
    return gates.Context(host=host, session_id=session, agent_id=payload.get("agent_id"),
                         tool_use_id=payload["tool_use_id"], transcript=t,
                         evidence=bundle, entries=tuple(entries))


def _save_users(payload: dict, host: str, messages: tuple[str, ...], keys: tuple[str, ...]) -> None:
    _append(payload, host, None, gates.Outcome("captured", record={
        "gate": "user_context", "messages": list(messages), "seen_user_keys": list(keys)}))


def _saved_users(payload: dict, host: str, t: transcript.Transcript, entries: list[dict]) -> tuple[str, ...]:
    raw_users = tuple(evidence.redact(message, _secrets()) for message in t.user_messages)
    raw_keys = t.user_keys or tuple(f"{i}:{message}" for i, message in enumerate(raw_users))
    scope = payload.get("agent_id")
    eligible_scopes = (None,) if scope is None else (None, scope)
    saved = next(((i, e) for i, e in reversed(list(enumerate(entries)))
                  if e.get("host") == host and e.get("gate") == "user_context"
                  and e.get("agent_id") in eligible_scopes), None)
    if saved is None:
        _save_users(payload, host, raw_users, raw_keys)
        return raw_users  # initial history comes from the host's protected transcript
    index, record = saved
    # Older snapshots can contain automatic feedback captured before the parser fix.
    users = [message for message in record["messages"]
             if host != "codex" or not transcript.is_codex_hook_feedback(message)]
    seen = list(record.get("seen_user_keys", ()))
    new_users = [(key, message) for key, message in zip(raw_keys, raw_users) if key not in seen]
    prompts = [entry.get("prompt") for entry in entries[index + 1:]
               if entry.get("host") == host and entry.get("gate") == "user_prompt"]
    reply_keys = set(t.question_reply_keys) if host == "codex" and set(seen).intersection(raw_keys) else set()
    for key, message in new_users:
        if message in prompts:
            prompts.remove(message)
        elif key not in reply_keys:
            continue
        users.append(message)
        seen.append(key)
    if tuple(users) != tuple(record["messages"]):
        _save_users(payload, host, tuple(users), tuple(seen))
    return tuple(users)


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
    parser.add_argument("event", choices=("pre_tool_use", "stop", "subagent_stop", "session_start", "user_prompt_submit"))
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
