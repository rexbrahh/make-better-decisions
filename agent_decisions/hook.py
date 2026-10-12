"""Hook entry point: one payload in, one response under the protected review policy."""

from __future__ import annotations

import argparse
import json
import os
import re
import shlex
import subprocess
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
        return _gate(payload, host, lambda ctx: gates.question_gate(tool_input, ctx, _asker(post, ctx.metrics)))
    if host == "claude" and name == "ExitPlanMode":
        plan = tool_input.get("plan")
        return _gate(payload, host, lambda ctx: gates.plan_gate(
            ctx, _asker(post, ctx.metrics), plan=plan if isinstance(plan, str) else None))
    if name in policy.READ_TOOLS:
        return None
    return _gate(payload, host, lambda ctx: gates.edit_gate(name, tool_input, ctx, _asker(post, ctx.metrics)))


def _decide(payload: dict, host: str, tool_input: dict, request: str, post) -> dict:
    ctx = None
    try:
        paths = json.loads(request).get("contracts", [])
        contracts = gates._request_scope(json.dumps({"scope": {"paths": paths}})).get("paths", [])[:8]
    except (ValueError, AttributeError, gates.RequestError):
        contracts = []  # gates.decide retains the existing invalid-request diagnostic.
    try:
        ctx = _context(payload, host, contracts=contracts)
    except OSError as error:
        turn = None
        next_step = "Decide yourself and state your evidence." if policy.fail_open(host) else \
            "Required review is unavailable. Restore readable host context and consult again."
        outcome = gates.Outcome("unverified", f"ts-decide: decision unverified (transcript unreadable: "
                                              f"{type(error).__name__}). " + next_step,
                                {"gate": "decide"})
    else:
        turn = ctx.transcript.turn_key
        if any(c.id == ctx.tool_use_id and c.output for c in ctx.transcript.tool_calls) or any(
                e.get("gate") == "decide" and e.get("tool_use_id") == ctx.tool_use_id for e in ctx.entries):
            return _deny("ts-decide: this tool call has already completed. Run a new, separate ts-decide call.")
        outcome = gates.decide(request, ctx, _asker(post, ctx.metrics))
    outcome = replace(outcome, message=evidence.redact(outcome.message, _secrets()))
    if outcome.record is not None:
        outcome = replace(outcome, record={**outcome.record, "verdict": outcome.message})
        _append(payload, host, turn, outcome, ctx=ctx)
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
        if not policy.fail_open(host):
            return _deny(f"Required review unavailable: transcript unreadable ({type(error).__name__}); "
                         "action not run. Restore readable host context and retry.")
        return _note(f"agent-decisions: gate skipped, transcript unreadable ({type(error).__name__})")
    outcome = run(ctx)
    if outcome.record is not None:
        _append(payload, host, ctx.transcript.turn_key, outcome, ctx=ctx)
    if outcome.action == "deny":
        return _deny(outcome.message)
    return _note(outcome.note) if outcome.note else None


def _stop(payload: dict, host: str, post) -> dict | None:
    try:
        ctx = _context(payload, host)
    except OSError as error:
        if not policy.fail_open(host):
            return {"decision": "block", "reason": f"Required final review unavailable: transcript unreadable "
                    f"({type(error).__name__}). Restore readable host context and retry."}
        return _note(f"agent-decisions: stop check skipped, transcript unreadable ({type(error).__name__})")
    final = payload.get("last_assistant_message")
    final = final if isinstance(final, str) else ctx.transcript.last_assistant_text
    outcome = gates.stop_gate(final, bool(payload.get("stop_hook_active")), ctx, _asker(post, ctx.metrics))
    if outcome.record is not None:
        _append(payload, host, ctx.transcript.turn_key, outcome, ctx=ctx)
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


def _context(payload: dict, host: str, *, contracts=()) -> gates.Context:
    started = time.perf_counter()
    t = _load_transcript(payload, host)
    metrics = {"transcript_ms": (time.perf_counter() - started) * 1_000}
    canonical = dict(payload)
    if t.identity is not None:
        canonical.update({"session_id": t.identity.session_id, "agent_id": t.identity.agent_id})
    secrets = _secrets()
    session = canonical.get("session_id") or "unknown"
    ledger_started = time.perf_counter()
    entries = ledger.entries(session)
    t = replace(t, user_messages=_saved_users(canonical, host, t, entries))
    metrics["ledger_ms"] = (time.perf_counter() - ledger_started) * 1_000
    repo_id = _repository_id(payload.get("cwd") or os.getcwd())
    _record_identity(canonical, host, t, entries, repo_id)
    evidence_started = time.perf_counter()
    bundle = evidence.build(t, secrets,
        query=json.dumps(payload.get("tool_input") or payload.get("last_assistant_message") or ""),
        cwd=payload.get("cwd") or os.getcwd(), contracts=contracts)
    bundle["metadata"]["user_history_source"] = "signed_host_context"
    bundle["metadata"]["source_root"] = str(Path(payload.get("cwd") or os.getcwd()).resolve())
    evidence._trim_to_budget(bundle)
    inherited = _inherited_decisions(t, entries, host, session, repo_id, secrets, canonical)
    ctx = gates.Context(host=host, session_id=session, agent_id=canonical.get("agent_id"),
                         tool_use_id=payload["tool_use_id"], transcript=t,
                         evidence=bundle, entries=tuple(entries), repo_id=repo_id,
                         inherited_entries=tuple(inherited), metrics=metrics)
    completed = gates.consulted(ctx)
    ledger.remember_architecture(repo_id, completed)
    history = ledger.architecture_entries(repo_id)
    contract_paths = [path for entry in [*reversed(completed), *reversed(history)] for path in entry.get("contracts", {})]
    source_paths = [item["path"] for entry in reversed(completed)
                    if entry.get("state", {}).get("scope", {}).get("paths")
                    for item in entry.get("state", {}).get("evidence", {}).get("source_files", ())
                    if item.get("trust") == "verified_file_observation"]
    current_files = {item["path"]: {"sha256": item["sha256"], "trust": item["trust"]}
                     for item in bundle.get("source_files", ()) if item.get("trust") == "verified_file_observation"}
    missing = [path for path in [*contract_paths, *source_paths] if path not in current_files]
    if missing:
        current_files.update(evidence.contract_observations(missing, payload.get("cwd") or os.getcwd(), secrets))
    if contract_paths or source_paths:
        bundle["file_observations"] = current_files
        bundle["contract_observations"] = {path: current_files[path] for path in dict.fromkeys(contract_paths)
                                           if path in current_files}
    if history:
        bundle["architecture_history"] = evidence.architecture_history(history, bundle.get("contract_observations", {}))
    evidence._trim_to_budget(bundle)
    metrics["evidence_ms"] = (time.perf_counter() - evidence_started) * 1_000
    metrics["context_ms"] = (time.perf_counter() - started) * 1_000
    return ctx


def _repository_id(cwd: str) -> str:
    root = Path(cwd).resolve()
    try:
        result = subprocess.run(["git", "-C", str(root), "rev-parse", "--git-common-dir"],
                                capture_output=True, text=True, timeout=1)
        if result.returncode == 0:
            return str((root / result.stdout.strip()).resolve())
    except (OSError, subprocess.TimeoutExpired):
        pass
    return str(root)


def _record_identity(payload: dict, host: str, t: transcript.Transcript, entries: list[dict], repo_id: str) -> None:
    identity = t.identity
    if identity is None:
        return
    record = {"gate": "host_identity", "thread_id": identity.thread_id,
              "transcript_path": str(Path(payload["transcript_path"]).resolve()), "repo_id": repo_id}
    if not any(entry.get("gate") == "host_identity" and entry.get("host") == host
               and all(entry.get(key) == value for key, value in record.items()) for entry in entries):
        _append(payload, host, None, gates.Outcome("observed", record=record))


def _inherited_decisions(t: transcript.Transcript, entries: list[dict], host: str,
                         session: str, repo_id: str, secrets: tuple[str, ...], payload: dict) -> list[dict]:
    identity = t.identity
    if t.brief is None:
        return []
    if host == "claude":
        agent_id = payload.get("agent_id")
        if not agent_id:
            return []
        try:
            parent = transcript.load(payload["transcript_path"], host)
        except OSError:
            return []
        expected_parent = None  # only the main transcript proves this Claude dispatch
    else:
        if identity is None or identity.parent_thread_id is None:
            return []
        registered = next((entry for entry in reversed(entries)
                           if entry.get("gate") == "host_identity" and entry.get("host") == host
                           and entry.get("thread_id") == identity.parent_thread_id
                           and entry.get("repo_id") == repo_id), None)
        if registered is None:
            return []
        try:
            parent = transcript.load(registered["transcript_path"], host)
        except OSError:
            return []
        if parent.identity is None or parent.identity.thread_id != identity.parent_thread_id \
                or parent.identity.session_id != session:
            return []
        agent_id = identity.agent_id
        parent_path = agent_id.rsplit("/", 1)[0]
        expected_parent = None if parent_path == "/root" else parent_path
        if parent.identity.agent_id != expected_parent:
            return []
    assigned = {}
    for call in parent.tool_calls:
        if call.name not in {"spawn_agent", "Agent", "Task"} or not call.output:
            continue
        try:
            args = json.loads(call.input)
        except (ValueError, TypeError):
            continue
        if not isinstance(args, dict):
            continue
        if host == "claude":
            footer = re.search(r"(?m)^agentId: ([A-Za-z0-9_-]{1,128}) \(use SendMessage[^\n]*\)\n"
                               r"<usage>[\s\S]*</usage>\s*$", call.output)
            message = args.get("prompt")
            verified = call.output.startswith("[Subagent hand-back]") and footer is not None \
                and footer[1] == agent_id and message == t.brief
        else:
            try:
                result = json.loads(call.output)
            except (ValueError, TypeError):
                continue
            message = args.get("message")
            verified = isinstance(result, dict) and result.get("task_name") == agent_id \
                and isinstance(message, str) and message in t.brief
        if not verified:
            continue
        accepted = re.search(r"(?m)^Accepted option:\s*([A-Za-z0-9_-]{1,40})\s*$", message)
        references = re.search(r"(?m)^Evidence refs:\s*([^\n]+)$", message)
        refs = [value.strip() for value in references[1].split(",")] if references is not None else []
        for receipt_id in re.findall(r"(?m)^Decision receipt:\s*([A-Za-z0-9_-]{1,128})\s*$", message):
            assigned[receipt_id] = (accepted[1] if accepted is not None else None, refs)
    if not assigned:
        return []
    parent_context = gates.Context(host, session, expected_parent, "parent-observation", parent,
                                  evidence.build(parent, secrets), tuple(entries), repo_id)
    observed_refs = {call.id for call in parent.tool_calls if call.output and not transcript.is_decide(call)}
    inherited = []
    for entry in gates.consulted(parent_context):
        if entry.get("receipt_id") not in assigned or entry.get("repo_id") != repo_id \
                or not entry.get("state", {}).get("scope", {}).get("paths"):
            continue
        accepted, refs = assigned[entry["receipt_id"]]
        selected = entry["pick"] if accepted is None else accepted
        if selected != entry["pick"] and (entry["action"] != "advisory"
                or not entry.get("eligibility_checked") or selected not in entry.get("eligible_options", ())
                or not refs or not set(refs).issubset(observed_refs)):
            continue
        if selected not in entry.get("state", {}).get("options", {}):
            continue
        inherited.append({**entry, "origin": "parent_dispatch", "source_action": entry["action"],
                          "action": "binding", "pick": selected, "parent_agent_id": expected_parent,
                          "inherited_by": agent_id, "accepted_evidence_refs": refs})
    return inherited


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


def _asker(post, metrics: dict | None = None):
    def ask(state, questions):
        key = jev.load_key()
        call_metrics = {} if metrics is not None else None
        request_state = evidence.scrub(state, (key,))
        try:
            return jev.ask(request_state, evidence.scrub(questions, (key,)),
                           key=key, post=post, metrics=call_metrics)
        except jev.JevError as error:
            if call_metrics is not None:
                call_metrics["error"] = error.reason
                if error.detail:
                    call_metrics["error_detail"] = error.detail
            raise
        finally:
            state.clear()
            state.update(request_state)
            if metrics is not None:
                metrics.setdefault("api_calls", []).append(call_metrics)
    return ask


def _append(payload: dict, host: str, turn: str | None, outcome: gates.Outcome,
            *, ctx: gates.Context | None = None) -> None:
    ledger.append({"time": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "host": host,
                   "session_id": ctx.session_id if ctx is not None else payload.get("session_id") or "unknown",
                   "agent_id": ctx.agent_id if ctx is not None else payload.get("agent_id"),
                   "tool_use_id": payload["tool_use_id"], "turn": turn, "action": outcome.action,
                   **({"metrics": evidence.scrub(ctx.metrics, _secrets())} if ctx is not None else {}),
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
    try:
        out = handle(args.event, json.load(sys.stdin), args.host)
    except Exception as error:
        try:
            availability = policy.fail_open(args.host)
        except Exception:
            availability = False
        message = f"agent-decisions: hook error ({type(error).__name__})"
        if availability:
            out = _note(message + "; action allowed under availability policy")
        elif args.event == "pre_tool_use":
            out = _deny(message + "; required review unavailable, action not run")
        elif args.event in {"stop", "subagent_stop"}:
            out = {"decision": "block", "reason": message + "; required final review unavailable"}
        else:
            out = _note(message + "; required review remains active")
    if os.environ.get("AGENT_DECISIONS_OFF") == "1":
        out = dict(out or {})
        out["systemMessage"] = (out.get("systemMessage", "") + " agent-decisions: AGENT_DECISIONS_OFF ignored; "
                                "review policy is controlled by the protected installer").strip()
    if out:
        json.dump(out, sys.stdout)
    return 0
