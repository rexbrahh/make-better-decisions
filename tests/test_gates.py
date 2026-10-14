from __future__ import annotations

import json
import os
import re
import shlex
from unittest import mock

from agent_decisions import gates, jev, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx

REQUEST = {"decision": "Bob's café: which cache?", "options": [
    {"id": "a", "description": "it's ü"}, {"id": "b", "description": "line\nbreak"}]}


class ClassifyTest(StateTestCase):
    def test_complete_non_shell_heredocs_keep_command_mentions_as_data(self):
        commands = (
            "python3 - <<'PY'\nif ('ts-decide' in text):\n    print(text)\nPY\n",
            "python3 - <<'PY'\nprint('ts-decide')\nPY\n",
            "node - <<'JS'\nconsole.log('ts-decide');\nJS\n",
            "cat <<'LOG'\n(ts-decide) is the diagnostic subject\nLOG\n",
            "# diagnostic\npython3 - <<'PY'\nprint('ts-decide')\nPY\n# finished\n",
        )
        for command in commands:
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("other", ""))

    def test_newline_and_shell_heredoc_consultations_remain_misuse(self):
        commands = (
            "echo status\nts-decide '{}'",
            "echo status # comment\nts-decide '{}'",
            "bash <<'SH'\nts-decide '{}'\nSH\n",
            "env -S 'bash -' <<'SH'\nts-decide '{}'\nSH\n",
            "python3 - <<'PY'\nprint('diagnostic')\nPY\nts-decide '{}'\n",
        )
        for command in commands:
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("misuse", ""))

    def test_shell_comments_and_quoted_or_escaped_newlines_are_data(self):
        for command in ("# (ts-decide) is the subject\nls", "echo 'literal\nts-decide'",
                        "echo foo \\\nts-decide '{}'"):
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("other", ""))

    def test_wrapped_consultations_are_misuse(self):
        commands = ("env ts-decide '{}'", "command ts-decide '{}'", "exec ts-decide '{}'",
                    "time ts-decide '{}'", "xargs ts-decide <<'EOF'\n{}\nEOF\n",
                    "env -S'bash -' <<'SH'\nts-decide '{}'\nSH\n",
                    "env --split-string='bash -' <<'SH'\nts-decide '{}'\nSH\n",
                    "env -i FLAG=1 command -p ts-decide '{}'", "FLAG=1 ts-decide '{}'",
                    "env -P /usr/bin ts-decide '{}'")
        for command in commands:
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("misuse", ""))

    def test_wrapped_lookups_mentions_and_data_heredocs_are_other(self):
        commands = ("command -v ts-decide", "command -V ts-decide", "command -pv ts-decide", "env echo ts-decide",
                    "exec -a ts-decide echo status", "time -o ts-decide echo status",
                    "xargs -I ts-decide echo ts-decide", "xargs -J ts-decide echo ts-decide",
                    "env -- FLAG=1 echo ts-decide",
                    "env python3 - <<'PY'\nprint('ts-decide')\nPY\n",
                    "env -S'python3 -' <<'PY'\nprint('ts-decide')\nPY\n")
        for command in commands:
            with self.subTest(command=command):
                self.assertEqual(gates.classify(command), ("other", ""))

    def test_python_heredoc_can_mention_the_consultation_command_as_data(self):
        body = "print('the user\\'s ts-decide history')\n"
        compile(body, "diagnostic", "exec")
        command = "python3 - <<'PY'\n" + body + "PY\n"
        self.assertEqual(gates.classify(command), ("other", ""))

    def test_whole_command_forms_carry_the_request_intact(self):
        raw = json.dumps(REQUEST, ensure_ascii=False)
        quoted = f"ts-decide {shlex.quote(raw)}"
        heredoc = f"ts-decide <<'EOF'\n{raw}\nEOF\n"
        for command in (quoted, heredoc, f"  {quoted}  ", f"/Users/x/agent-decisions/bin/ts-decide {shlex.quote(raw)}"):
            with self.subTest(command=command[:30]):
                kind, request = gates.classify(command)
                self.assertEqual(kind, "decide")
                self.assertEqual(json.loads(request), REQUEST)

    def test_crlf_heredoc_is_accepted(self):
        raw = json.dumps(REQUEST)
        kind, request = gates.classify(f"ts-decide <<'EOF'\r\n{raw}\r\nEOF\r\n")
        self.assertEqual(kind, "decide")
        self.assertEqual(json.loads(request), REQUEST)

    def test_ts_decide_inside_a_larger_command_is_misuse(self):
        for command in ("ts-decide '{}' && rm -rf x", "ts-decide '{}'; ls", "ts-decide '{}' | tee out",
                        "ts-decide '{}' > out", "cd /x && ts-decide '{}'", "(ts-decide '{}')",
                        "ts-decide '{}'\nls", "ts-decide '{}' extra", "ts-decide \"$(cat req.json)\" | cat",
                        "ts-decide '{}' 'unterminated"):
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
    def test_peer_python_list_read_does_not_raise_an_invalid_glob_error(self):
        command = """python3 - <<'PY'
import hashlib,json
from pathlib import Path
root=Path('/Users/rexliu/fieldforge/.local/v45-campaign-20261004')
for p in [root/'journal.jsonl',root/'qualification-20261005/QUALIFICATION.md']:
    data=p.read_bytes()
    print(str(p),hashlib.sha256(data).hexdigest())
    if p.suffix=='.jsonl':
        rows=[json.loads(x) for x in data.decode().splitlines() if x.strip()]
        print(json.dumps(rows[-1],indent=2))
    else:print('\\n'.join(data.decode().splitlines()[:42]))
PY
"""
        self.assertIsNone(gates.guard({"cmd": command}, cwd="/Users/rexliu/fieldforge"))

    def test_invalid_glob_does_not_skip_a_later_private_path(self):
        # Python versions differ in how fnmatch handles invalid bracket ranges.
        with mock.patch.object(gates.fnmatch, "fnmatchcase", side_effect=re.error("bad character range")):
            outcome = gates.guard({"paths": ["/tmp/[z-a]", str(policy.state_dir() / "sessions/s.jsonl")]})
        self.assertEqual(outcome.action, "deny")

    def test_denies_any_mention_of_the_state_directory(self):
        state = str(policy.state_dir())
        for tool_input in ({"command": f"echo x >> {state}/sessions/s.jsonl"},
                           {"file_path": f"{state}/verdicts/v.txt", "content": "x"},
                           {"command": "cat ~/.local/state/agent-decisions/sessions/s.jsonl"}):
            with self.subTest(tool_input=tool_input):
                outcome = gates.guard(tool_input)
                self.assertEqual(outcome.action, "deny")
                self.assertEqual(outcome.record, {"gate": "guard"})

    def test_glob_and_shell_spellings_of_the_state_path_are_denied(self):
        state = str(policy.state_dir())
        for command in (f"cat {state[:-3]}*/sessions/s.jsonl",
                        "cat ~/.local/state/agent-d*/ledger.key", "cat ~/.local/state/*/ledger.key",
                        "cat ~/.local/**/ledger.key", "cat ~/.local/sta?e/agent-decisions/ledger.key",
                        "cd ~ && cat .local/state/agent-decisions/ledger.key",
                        "cat $HOME/.local/state/agent-decisions/ledger.key",
                        "cat ${HOME}/.local/state/agent-decisions/ledger.key",
                        "cat ~/.local/state/agent-{decisions,x}/ledger.key"):
            with self.subTest(command=command):
                self.assertEqual(gates.guard({"command": command}).action, "deny")

    def test_globs_that_stop_above_the_state_path_are_allowed_like_literal_ancestors(self):
        users = os.path.dirname(os.path.expanduser("~"))
        for command in ("ls ~/*", f"du -sh {users}/*", "ls ~/.local/*", "ls ~/.local/state",
                        "echo $HOME_DIR/x", "ls ~/agent-decisions/{README,SECURITY}.md"):
            with self.subTest(command=command):
                self.assertIsNone(gates.guard({"command": command}))

    def test_tilde_words_for_unknown_users_do_not_raise(self):
        self.assertIsNone(gates.guard({"command": "echo ~5% faster; ls ~nosuchuser/x"}))
        self.assertIsNone(gates.guard({"file_path": "~nosuchuser/x"}))

    def test_allows_the_repository_and_everything_else(self):
        self.assertIsNone(gates.guard({"command": "cd ~/agent-decisions && git status"}))
        self.assertIsNone(gates.guard({"file_path": os.path.expanduser("~/agent-decisions/README.md")}))


ASKED = {"questions": [{"question": "Which database for the job queue?", "header": "DB", "multiSelect": False,
                        "options": [{"label": "Postgres", "description": "x"}, {"label": "SQLite", "description": "y"}]}]}


class QuestionGateTest(StateTestCase):
    def test_oversized_question_is_rejected_before_the_api(self):
        fake = FakeJev()
        outcome = gates.question_gate({"questions": [{"title": "x" * 250_000}]}, make_ctx(), asker(fake))
        self.assertEqual(outcome.action, "deny")
        self.assertEqual(fake.requests, [])

    def test_codex_question_shape_preserves_prompt_and_string_options(self):
        fake = FakeJev(stated_0=0.95, owner_0="user")
        outcome = gates.question_gate({"questions": [{"title": "Which database?", "options": ["Postgres", "SQLite"]}]},
                                      make_ctx(host="codex"), asker(fake))
        self.assertEqual(outcome.action, "deny")
        self.assertEqual(fake.requests[0]["state"]["questions"],
                         [{"question": "Which database?", "options": ["Postgres", "SQLite"]}])

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

    def test_identical_denial_is_cached_and_still_denied(self):
        first = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(stated_0=0.9)))
        fake = FakeJev(stated_0=0.9)
        second = gates.question_gate(ASKED, make_ctx(entries=[{**first.record, "action": "deny"}]), asker(fake))
        self.assertEqual(second.action, "deny")
        self.assertEqual(fake.requests, [])

    def test_jev_failure_allows_with_a_note_in_availability_mode(self):
        self.availability()
        outcome = gates.question_gate(ASKED, make_ctx(), asker(FakeJev(raises=jev.JevError("http_529"))))
        self.assertEqual(outcome.action, "allow")
        self.assertIn("http_529", outcome.note)


class PlanGateTest(StateTestCase):
    def test_every_retry_needs_a_completed_consultation(self):
        ctx = make_ctx()
        first = gates.plan_gate(ctx)
        self.assertEqual(first.action, "deny")
        again = make_ctx(entries=[{**first.record, "action": "deny", "turn": ctx.transcript.turn_key}])
        self.assertEqual(gates.plan_gate(again).action, "deny")

    def test_allows_after_a_backed_ts_decide(self):
        calls = (ToolCall("toolu_d", "Bash", '{"command": "ts-decide \'{}\'"}', "ts-decide verdict: ADVISORY"),)
        ctx = make_ctx(Transcript(("plan it",), calls, calls), entries=[{"gate": "decide", "tool_use_id": "toolu_d", "action": "advisory"}])
        self.assertEqual(gates.plan_gate(ctx).action, "allow")

    def test_a_forged_ledger_line_does_not_count(self):
        ctx = make_ctx(entries=[{"gate": "decide", "tool_use_id": "toolu_forged"}])
        self.assertEqual(gates.plan_gate(ctx).action, "deny")
