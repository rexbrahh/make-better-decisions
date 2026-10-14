"""Transcript record builders that copy real record shapes (observed 2026-10-10), and a fake Jev."""

from __future__ import annotations

import json
import os
import shutil
import tempfile
import unittest
from pathlib import Path


def human(text):
    return {"type": "user", "message": {"role": "user", "content": text}, "origin": {"kind": "human"}}


def peer(text):
    return {"type": "user", "message": {"role": "user", "content": text}, "origin": {"kind": "peer"}}


def sidechain_user(text):
    return {"type": "user", "isSidechain": True, "agentId": "a1", "message": {"role": "user", "content": text}}


def tool_use(call_id, name, tool_input):
    return {"type": "assistant", "message": {"role": "assistant", "content": [
        {"type": "tool_use", "id": call_id, "name": name, "input": tool_input}]}}


def tool_result(call_id, content):
    return {"type": "user", "message": {"role": "user", "content": [
        {"type": "tool_result", "tool_use_id": call_id, "content": content}]}}


def assistant_text(text):
    return {"type": "assistant", "message": {"role": "assistant", "content": [{"type": "text", "text": text}]}}


def queued(text, kind="human", human_turn=True, mode="prompt"):
    """A message queued while the agent works (observed 2026-10-14). Peers and notifications queue the same way."""
    attachment = {"type": "queued_command", "commandMode": mode, "prompt": text, "origin": {"kind": kind}}
    if human_turn:
        attachment["humanTurn"] = True
    return {"type": "attachment", "attachment": attachment}


def ask(call_id, *questions):
    return tool_use(call_id, "AskUserQuestion", {"questions": [
        {"question": q, "header": "Choice", "multiSelect": False,
         "options": [{"label": "Keep it", "description": ""}, {"label": "Drop it", "description": ""}]}
        for q in questions]})


def answered(call_id, answers, source="user_temporary"):
    """An AskUserQuestion result (observed 2026-10-14). Claude Code 2.1.295+ records a human pick as user_temporary."""
    record = tool_result(call_id, "Your questions have been answered.")
    record["toolUseResult"] = {"questions": [], "answers": answers, "annotations": {}}
    if source is not None:
        record["permissionDecision"] = {"decision": "accept", "source": source}
    return record


def codex_user(text):
    return {"type": "response_item", "payload": {
        "type": "message", "role": "user", "content": [{"type": "input_text", "text": text}]}}


def codex_assistant(text):
    return {"type": "response_item", "payload": {
        "type": "message", "role": "assistant", "content": [{"type": "output_text", "text": text}]}}


def codex_call(call_id, name, arguments):
    return {"type": "response_item", "payload": {
        "type": "function_call", "id": f"fc_{call_id}", "name": name, "arguments": arguments, "call_id": call_id}}


def codex_custom(call_id, name, tool_input):
    return {"type": "response_item", "payload": {
        "type": "custom_tool_call", "id": f"ctc_{call_id}", "status": "completed",
        "call_id": call_id, "name": name, "input": tool_input}}


def codex_output(call_id, output, kind="function_call_output"):
    return {"type": "response_item", "payload": {"type": kind, "call_id": call_id, "output": output}}


def write_jsonl(directory, name, records, tail=""):
    path = Path(directory) / name
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("".join(json.dumps(r) + "\n" for r in records) + tail)
    return path


class StateTestCase(unittest.TestCase):
    """Gives each test its own temporary directory and agent-decisions state directory."""

    def setUp(self):
        self.dir = Path(tempfile.mkdtemp()).resolve()
        self.addCleanup(shutil.rmtree, self.dir, ignore_errors=True)
        self._saved = {k: os.environ.get(k) for k in ("AGENT_DECISIONS_STATE", "TYPESAFE_API_KEY", "AGENT_DECISIONS_OFF",
                                                      "CLAUDE_CONFIG_DIR")}
        os.environ["AGENT_DECISIONS_STATE"] = str(self.dir / "state")
        os.environ["TYPESAFE_API_KEY"] = "test-key-0123456789abcdef"
        os.environ.pop("AGENT_DECISIONS_OFF", None)
        os.environ.pop("CLAUDE_CONFIG_DIR", None)
        self.addCleanup(self._restore)

    def _restore(self):
        for key, value in self._saved.items():
            if value is None:
                os.environ.pop(key, None)
            else:
                os.environ[key] = value

    def availability(self, host="claude"):
        from agent_decisions import ledger
        ledger.append({"session_id": "installation", "gate": "installation", "action": "configured",
                       "spec": {"host": host, "review_mode": "availability"}})


class FakeJev:
    """Stands in for the HTTP transport. A noul answers its override or 0.0;
    a choice answers its override or its first criteria key, with probability 1."""

    def __init__(self, raises=None, raw=None, **answers):
        self.answers = answers
        self.raises = raises
        self.raw = raw
        self.requests = []

    def __call__(self, body, key, timeout):
        request = json.loads(body)
        self.requests.append(request)
        if self.raises is not None:
            raise self.raises
        if self.raw is not None:
            return self.raw
        answers = {}
        for qid, question in request["questions"].items():
            want = self.answers.get(qid)
            if question["type"] == "noul":
                answers[qid] = {"type": "noul", "noul": 0.0 if want is None else want}
            else:
                options = list(question["criteria"])
                choice = options[0] if want is None else want
                answers[qid] = {"type": "choice", "choice": choice, "confidence": 1.0,
                                "probabilities": {o: 1.0 if o == choice else 0.0 for o in options}}
        return json.dumps({"model": "jev-1.13.0", "answers": answers, "usage": {}}).encode()


def make_ctx(t=None, *, entries=(), agent_id=None, host="claude", tool_use_id="toolu_now"):
    from agent_decisions import evidence, gates
    from agent_decisions.transcript import Transcript

    t = t or Transcript(("decide the cache policy",), (), ())
    calls = {call.id: call for call in t.tool_calls}
    records = [{"host": host, "verdict": calls[e["tool_use_id"]].output,
                **e} if e.get("gate") == "decide" and e.get("tool_use_id") in calls
               else {"host": host, **e} for e in entries]
    return gates.Context(host=host, session_id="s1", agent_id=agent_id, tool_use_id=tool_use_id,
                         transcript=t, evidence=evidence.build(t), entries=tuple(records))


def asker(fake):
    from agent_decisions import jev

    return lambda state, questions: jev.ask(state, questions, key="k", post=fake)
