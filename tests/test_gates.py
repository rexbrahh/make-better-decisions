from __future__ import annotations

import json
import os
import shlex

from agent_decisions import gates, jev, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx

REQUEST = {"decision": "Bob's café: which cache?", "options": [
    {"id": "a", "description": "it's ü"}, {"id": "b", "description": "line\nbreak"}]}


class ClassifyTest(StateTestCase):
    def test_whole_command_forms_carry_the_request_intact(self):
        raw = json.dumps(REQUEST, ensure_ascii=False)
        quoted = f"ts-decide {shlex.quote(raw)}"
        heredoc = f"ts-decide <<'EOF'\n{raw}\nEOF\n"
        for command in (quoted, heredoc, f"  {quoted}  ", f"/Users/x/agent-decisions/bin/ts-decide {shlex.quote(raw)}"):
            with self.subTest(command=command[:30]):
                kind, request = gates.classify(command)
                self.assertEqual(kind, "decide")
                self.assertEqual(json.loads(request), REQUEST)

    def test_ts_decide_inside_a_larger_command_is_misuse(self):
        for command in ("ts-decide '{}' && rm -rf x", "ts-decide '{}'; ls", "ts-decide '{}' | tee out",
                        "ts-decide '{}' > out", "cd /x && ts-decide '{}'", "(ts-decide '{}')",
                        "ts-decide '{}'\nls", "ts-decide '{}' extra", "ts-decide \"$(cat req.json)\" | cat"):
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command)[0], "misuse")

    def test_mentions_that_do_not_invoke_ts_decide_are_other(self):
        for command in ("grep -rn ts-decide .", "echo ts-decide", "ls", "git log --grep ts-decide"):
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("other", ""))

    def test_command_of_reads_claude_and_codex_shapes(self):
        self.assertEqual(gates.command_of({"command": "ls"}), "ls")
        self.assertEqual(gates.command_of({"cmd": "ls -l"}), "ls -l")
        self.assertEqual(gates.command_of({"command": ["bash", "-lc", "ts-decide '{}'"]}), "ts-decide '{}'")
        self.assertEqual(gates.command_of({"command": ["ls", "-l"]}), "ls -l")
        self.assertEqual(gates.command_of({"input": "patch"}), "")


class GuardTest(StateTestCase):
    def test_denies_any_mention_of_the_state_directory(self):
        state = str(policy.state_dir())
        for tool_input in ({"command": f"echo x >> {state}/sessions/s.jsonl"},
                           {"file_path": f"{state}/verdicts/v.txt", "content": "x"},
                           {"command": "cat ~/.local/state/agent-decisions/sessions/s.jsonl"}):
            with self.subTest(tool_input=tool_input):
                outcome = gates.guard(tool_input)
                self.assertEqual(outcome.action, "deny")
                self.assertEqual(outcome.record, {"gate": "guard"})

    def test_allows_the_repository_and_everything_else(self):
        self.assertIsNone(gates.guard({"command": "cd ~/agent-decisions && git status"}))
        self.assertIsNone(gates.guard({"file_path": os.path.expanduser("~/agent-decisions/README.md")}))


ASKED = {"questions": [{"question": "Which database for the job queue?", "header": "DB", "multiSelect": False,
                        "options": [{"label": "Postgres", "description": "x"}, {"label": "SQLite", "description": "y"}]}]}


class QuestionGateTest(StateTestCase):
    def test_denies_when_the_user_already_stated_the_answer(self):
        outcome = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(stated_0=0.9, owner_0="user")))
        self.assertEqual(outcome.action, "deny")
        self.assertIn("already answer", outcome.message)

    def test_denies_when_the_agent_owns_the_decision(self):
        outcome = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(stated_0=0.1, owner_0="agent")))
        self.assertEqual(outcome.action, "deny")
        self.assertIn("your call", outcome.message)

    def test_allows_a_user_owned_unanswered_question(self):
        outcome = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(stated_0=0.1, owner_0="user")))
        self.assertEqual(outcome.action, "allow")
        self.assertEqual(outcome.record["gate"], "question")

    def test_one_denial_per_question_set(self):
        first = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(stated_0=0.9)))
        fake = FakeJev(stated_0=0.9)
        second = gates.question_gate(ASKED, make_ctx(entries=[{**first.record, "action": "deny"}]), asker(fake))
        self.assertEqual(second.action, "allow")
        self.assertEqual(fake.requests, [])

    def test_jev_failure_allows_with_a_note(self):
        outcome = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(raises=jev.JevError("http_529"))))
        self.assertEqual(outcome.action, "allow")
        self.assertIn("http_529", outcome.note)


class PlanGateTest(StateTestCase):
    def test_denies_once_per_turn_without_ts_decide(self):
        ctx = make_ctx()
        first = gates.plan_gate(ctx)
        self.assertEqual(first.action, "deny")
        again = make_ctx(entries=[{**first.record, "action": "deny", "turn": ctx.transcript.turn_key}])
        self.assertEqual(gates.plan_gate(again).action, "allow")

    def test_allows_after_a_backed_ts_decide(self):
        calls = (ToolCall("toolu_d", "Bash", '{"command": "ts-decide \'{}\'"}'),)
        ctx = make_ctx(Transcript(("plan it",), calls, calls), entries=[{"gate": "decide", "tool_use_id": "toolu_d"}])
        self.assertEqual(gates.plan_gate(ctx).action, "allow")

    def test_a_forged_ledger_line_does_not_count(self):
        ctx = make_ctx(entries=[{"gate": "decide", "tool_use_id": "toolu_forged"}])
        self.assertEqual(gates.plan_gate(ctx).action, "deny")
