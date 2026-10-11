"""Regression tests for the final-review findings (guard evasion, fail-open holes, leaks, cheats)."""

from __future__ import annotations

import json
import shlex
import time
from unittest import mock

from agent_decisions import evidence, gates, hook, ledger, policy
from agent_decisions.transcript import ToolCall, Transcript, is_decide
from tests.helpers import FakeJev, StateTestCase, asker, human, make_ctx, tool_result, tool_use, write_jsonl

SECRET = "sk-abcdefghijklmnopqrstuvwx"
REQUEST = {"decision": "Which cache?", "options": [{"id": "lru", "description": "LRU"},
                                                   {"id": "lfu", "description": "LFU"}],
           "facts": []}


class GuardTest(StateTestCase):
    def test_indirect_and_case_variant_paths_are_denied(self):
        for command in ("cd ~/.local/state && cat agent-decisions/sessions/*",
                        "ls ~/.local/state/agent*",
                        "cat ~/.Local/State/Agent-Decisions/sessions/x"):
            with self.subTest(command=command):
                self.assertIsNotNone(gates.guard({"command": command}))

    def test_read_tools_are_guarded(self):
        payload = {"session_id": "s1", "tool_name": "Read", "tool_use_id": "toolu_r",
                   "tool_input": {"file_path": f"{policy.state_dir()}/sessions/s1.jsonl"}}
        out = hook.handle("pre_tool_use", payload, "claude", post=FakeJev())
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")

    def test_denial_survives_an_unwritable_ledger(self):
        payload = {"session_id": "s1", "tool_name": "Bash", "tool_use_id": "toolu_g",
                   "tool_input": {"command": "cat ~/.local/state/agent-decisions/sessions/s1.jsonl"}}
        with mock.patch.object(ledger, "append", side_effect=OSError("read-only")):
            out = hook.handle("pre_tool_use", payload, "claude", post=FakeJev())
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")


class LedgerEncodingTest(StateTestCase):
    def test_lone_surrogate_is_written_and_read_back(self):
        ledger.append({"session_id": "s1", "gate": "decide", "text": "\ud800"})
        self.assertEqual(ledger.entries("s1")[0]["text"], "\ud800")
        self.assertIn("\\ud800", ledger.write_verdict("toolu_s", "x \ud800").read_text())


class RedactionTest(StateTestCase):
    def test_option_descriptions_are_redacted_in_questions_and_verdict(self):
        path = write_jsonl(self.dir, "s1.jsonl", [human("Choose a config reader.")])
        request = {"decision": "Which config reader?", "options": [
            {"id": "a", "description": f"Use token {SECRET}"}, {"id": "b", "description": "Use environment"}],
            "facts": []}
        fake = FakeJev(pick="b", sufficient=0.9, owner="agent")
        out = hook.handle("pre_tool_use", {"session_id": "s1", "transcript_path": str(path),
                          "tool_name": "Bash", "tool_use_id": "redact1",
                          "tool_input": {"command": f"ts-decide {shlex.quote(json.dumps(request))}"}},
                          "claude", post=fake)
        self.assertNotIn(SECRET, json.dumps(fake.requests))
        command = out["hookSpecificOutput"]["updatedInput"]["command"]
        self.assertNotIn(SECRET, shlex.split(command)[2])

    def test_json_style_secret_is_redacted(self):
        self.assertNotIn("abcdefghijkl1234", evidence.redact('{"api_key": "abcdefghijkl1234"}'))

    def test_unterminated_key_blocks_redact_in_linear_time(self):
        text = "-----BEGIN PRIVATE KEY-----\nabc\n" * 20_000
        start = time.monotonic()
        evidence.redact(text)
        self.assertLess(time.monotonic() - start, 2.0)

    def test_agent_written_facts_are_redacted_before_jev_and_the_ledger(self):
        directory = self.dir
        path = write_jsonl(directory, "s1.jsonl", [
            human("speed up the tile cache"),
            tool_use("toolu_1", "Bash", {"command": "env"}),
            tool_result("toolu_1", f"export TOKEN {SECRET}"),
        ])
        fake = FakeJev(pick="lfu", sufficient=0.9, owner="agent")
        hook.handle("pre_tool_use", {"session_id": "s1", "transcript_path": str(path), "tool_name": "Bash",
                                     "tool_use_id": "toolu_d",
                                     "tool_input": {"command": f"ts-decide {shlex.quote(json.dumps(REQUEST))}"}},
                    "claude", post=fake)
        self.assertNotIn(SECRET, json.dumps(fake.requests))
        self.assertNotIn(SECRET, (policy.state_dir() / "sessions" / "s1.jsonl").read_text())


class ConsultedTest(StateTestCase):
    def test_a_comment_naming_ts_decide_does_not_hide_a_call(self):
        self.assertFalse(is_decide(ToolCall("a", "Bash", '{"command": "grep -rn foo src  # ts-decide"}')))

    def test_only_a_real_verdict_counts_as_consulted(self):
        def ctx(action):
            output = "ts-decide: decision unverified (jev timeout)" if action == "unverified" \
                else "ts-decide verdict: " + action.upper().replace("_", " ")
            call = ToolCall("toolu_d", "Bash", '{"command": "ts-decide \'{}\'"}', output)
            return make_ctx(Transcript(("fix",), (call,), (call,)),
                            entries=[{"gate": "decide", "tool_use_id": "toolu_d", "action": action}])
        for action, counts in (("refused", False), ("gather_evidence", False), ("unverified", True),
                               ("advisory", True), ("binding", True), ("ask_user", True)):
            with self.subTest(action=action):
                self.assertEqual(bool(gates.consulted(ctx(action))), counts)


class StopPerAgentTest(StateTestCase):
    def test_a_subagent_block_does_not_spend_the_main_agents_block(self):
        t = Transcript(("fix the cache",), (), ())
        entries = [{"gate": "stop", "action": "block", "turn": t.turn_key, "agent_id": "a1"}]
        outcome = gates.stop_gate("I chose LRU over LFU.", False, make_ctx(t, entries=entries),
                                  asker(FakeJev(made_choice=0.9)))
        self.assertEqual(outcome.action, "block")
