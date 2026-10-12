from __future__ import annotations

import json
from dataclasses import replace

from agent_decisions import gates, hook, jev, ledger
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, codex_user, human, make_ctx, write_jsonl


class EditGateTest(StateTestCase):
    def context(self, action="advisory", agent_id=None):
        call = ToolCall("decide1", "Bash", json.dumps({"command": "ts-decide '{}'"}),
                        "ts-decide verdict: " + action.upper().replace("_", " "))
        t = Transcript(("Fix the parser.",), (call,), (call,))
        entry = {"gate": "decide", "action": action, "tool_use_id": call.id,
                 "agent_id": agent_id, "decision": "How should the parser read input?",
                 "pick": "stream", "state": {"options": {"stream": "Read chunks"}}}
        return make_ctx(t, entries=(entry,))

    def run_gate(self, ctx=None, name="Edit", **answers):
        return gates.edit_gate(name, {"file_path": "parser.py", "new_string": "read_chunks()"},
                               ctx or make_ctx(), asker(FakeJev(**answers)))

    def test_discretionary_edit_without_consultation_is_denied_on_every_attempt(self):
        ctx = make_ctx()
        for _ in range(2):
            outcome = self.run_gate(ctx, needs_decision=0.95)
            self.assertEqual(outcome.action, "deny")
            self.assertIn("ts-decide", outcome.message)
            ctx = make_ctx(entries=(*ctx.entries, {**outcome.record, "action": outcome.action}))

    def test_exact_user_instruction_is_judged_and_allowed(self):
        fake = FakeJev(needs_decision=0.01)
        outcome = gates.edit_gate("Write", {"file_path": "README.md", "content": "Exact text"},
                                  make_ctx(), asker(fake))
        self.assertEqual(outcome.action, "allow")
        self.assertEqual(outcome.record["gate"], "edit")
        self.assertEqual(fake.requests[0]["state"]["pending_tool"], "Write")

    def test_covered_edit_is_allowed_but_unrelated_edit_is_denied(self):
        ctx = self.context()
        self.assertEqual(self.run_gate(ctx, needs_decision=0.95, covered=0.95).action, "allow")
        self.assertEqual(self.run_gate(ctx, needs_decision=0.95, covered=0.05).action, "deny")

    def test_main_agent_advice_covers_a_follow_up_without_binding_certainty(self):
        self.assertEqual(self.run_gate(self.context(), needs_decision=.95, covered=.76).action, "allow")
        worker = replace(self.context(action="binding", agent_id="worker"), agent_id="worker")
        self.assertEqual(len(gates.consulted(worker)), 1)
        self.assertEqual(self.run_gate(worker, needs_decision=.95, covered=.76).action, "deny")

    def test_unread_or_inactive_verdict_cannot_authorize_an_edit(self):
        for output in ("", "agent-decisions hook inactive: decision unverified",
                       "ts-decide verdict: ASK USER"):
            with self.subTest(output=output):
                ctx = self.context()
                call = replace(ctx.transcript.tool_calls[0], output=output)
                ctx = replace(ctx, transcript=replace(ctx.transcript, tool_calls=(call,), turn_calls=(call,)))
                fake = FakeJev(needs_decision=0.95, covered=0.99)
                outcome = gates.edit_gate("Edit", {}, ctx, asker(fake))
                self.assertEqual(outcome.action, "deny")
                self.assertEqual(fake.requests[0]["state"]["decisions"], [])

    def test_parent_or_unresolved_verdict_cannot_authorize_an_edit(self):
        for action, agent in (("advisory", "other-agent"), ("ask_user", None),
                              ("refused", None), ("gather_evidence", None)):
            fake = FakeJev(needs_decision=0.95, covered=0.99)
            outcome = gates.edit_gate("Edit", {}, self.context(action, agent), asker(fake))
            self.assertEqual(outcome.action, "deny")
            self.assertEqual(fake.requests[0]["state"]["decisions"], [])

    def test_shell_write_is_classified_and_gated(self):
        self.assertEqual(self.run_gate(name="Bash", writes_files=0.95, needs_decision=0.95).action, "deny")
        self.assertEqual(self.run_gate(name="Bash", writes_files=0.01, needs_decision=0.95).action, "allow")

    def test_failure_blocks_action_under_required_policy(self):
        outcome = gates.edit_gate("Edit", {}, make_ctx(), asker(FakeJev(raises=jev.JevError("timeout"))))
        self.assertEqual(outcome.action, "deny")
        self.assertIn("timeout", outcome.message)


class EditHookTest(StateTestCase):
    def setUp(self):
        super().setUp()
        self.path = write_jsonl(self.dir, "transcript.jsonl", [human("Fix the parser.")])
        self.codex_path = write_jsonl(self.dir, "rollout.jsonl", [codex_user("Fix the parser.")])

    def payload(self, name, value, host="claude"):
        return {"session_id": "s1", "tool_use_id": "edit1", "tool_name": name,
                "tool_input": value, "transcript_path": str(self.codex_path if host == "codex" else self.path)}

    def test_each_host_edit_shape_reaches_gate(self):
        for host, name, value in (("claude", "Edit", {"file_path": "parser.py"}),
                                  ("claude", "Write", {"file_path": "README.md"}),
                                  ("codex", "apply_patch", {"command": "*** Begin Patch\n*** End Patch"}),
                                  ("codex", "apply_patch", "*** Begin Patch\n*** End Patch")):
            with self.subTest(host=host, name=name, value=value):
                out = hook.handle("pre_tool_use", self.payload(name, value, host), host,
                                  post=FakeJev(needs_decision=0.95))
                self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(len([e for e in ledger.entries("s1") if e["gate"] == "edit"]), 4)

    def test_guard_takes_precedence_without_jev(self):
        fake = FakeJev()
        out = hook.handle("pre_tool_use", self.payload("Write", {"file_path": "~/.local/state/agent-decisions/x"}),
                          "claude", post=fake)
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(fake.requests, [])

    def test_shell_calls_reach_classifier_but_reads_do_not(self):
        fake = FakeJev(writes_files=0.95, needs_decision=0.95)
        out = hook.handle("pre_tool_use", self.payload("Bash", {"command": "sed -i '' s/old/new/ parser.py"}, "codex"),
                          "codex", post=fake)
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIsNone(hook.handle("pre_tool_use", self.payload("Read", {"file_path": "parser.py"}),
                                      "claude", post=fake))
        self.assertEqual(len(fake.requests), 1)

    def test_unreadable_transcript_fails_open_in_availability_mode(self):
        self.availability()
        payload = {**self.payload("Edit", {}), "transcript_path": str(self.dir / "missing")}
        out = hook.handle("pre_tool_use", payload, "claude", post=FakeJev(needs_decision=1.0))
        self.assertIn("transcript unreadable", out["systemMessage"])

    def test_empty_or_unparseable_transcript_fails_open_in_availability_mode_without_jev(self):
        for host in ("claude", "codex"):
            self.availability(host)
            for content in ("", "not json\n", "{}\n"):
                with self.subTest(host=host, content=content):
                    (self.codex_path if host == "codex" else self.path).write_text(content)
                    fake = FakeJev(needs_decision=1.0)
                    out = hook.handle("pre_tool_use", self.payload("Edit", {}, host), host, post=fake)
                    self.assertIn("transcript unreadable", out.get("systemMessage", ""))
                    self.assertEqual(fake.requests, [])

    def test_codex_question_tool_is_checked_before_sending(self):
        for name in ("request_user_input", "request_user_input_async"):
            payload = {**self.payload(name, {"questions": [{"question": "Which database?", "options": []}]}, "codex"),
                       "session_id": name}
            out = hook.handle("pre_tool_use", payload, "codex", post=FakeJev(stated_0=0.95, owner_0="user"))
            self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
