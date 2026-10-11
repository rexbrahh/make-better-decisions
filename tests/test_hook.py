from __future__ import annotations

import json
import os
import shlex
import subprocess
import sys
from pathlib import Path

from agent_decisions import hook, jev, ledger, policy
from tests.helpers import FakeJev, StateTestCase, codex_user, human, tool_result, tool_use, write_jsonl

ROOT = Path(__file__).resolve().parent.parent
REQUEST = {"decision": "Which cache?", "options": [{"id": "lru", "description": "LRU"},
                                                   {"id": "lfu", "description": "LFU"}]}
CONFIDENT = dict(pick="lfu", sufficient=0.9, owner="agent")


class HookTest(StateTestCase):
    def setUp(self):
        super().setUp()
        self.transcript = write_jsonl(self.dir, "s1.jsonl", [
            human("speed up the tile cache"),
            tool_use("toolu_1", "Bash", {"command": "trace"}),
            tool_result("toolu_1", "tiles are re-read in bursts"),
        ])

    def payload(self, command, **extra):
        return {"session_id": "s1", "transcript_path": str(self.transcript), "tool_name": "Bash",
                "tool_use_id": "toolu_d", "tool_input": {"command": command, "description": "decide"}, **extra}

    def verdict_output(self, out, key="command"):
        command = out["hookSpecificOutput"]["updatedInput"][key]
        return subprocess.run(["/bin/sh", "-c", command], cwd=self.dir,
                              capture_output=True, text=True, check=True).stdout

    def test_claude_ts_decide_returns_literal_verdict_stdout(self):
        out = hook.handle("pre_tool_use", self.payload(f"ts-decide {shlex.quote(json.dumps(REQUEST))}"),
                          "claude", post=FakeJev(**CONFIDENT))
        specific = out["hookSpecificOutput"]
        self.assertEqual(specific["permissionDecision"], "allow")
        self.assertEqual(specific["updatedInput"]["description"], "decide")
        output = self.verdict_output(out)
        self.assertIn("ts-decide verdict: ADVISORY", output)
        [entry] = [e for e in ledger.entries("s1") if e["gate"] == "decide"]
        self.assertEqual((entry["gate"], entry["action"], entry["tool_use_id"]), ("decide", "advisory", "toolu_d"))
        self.assertEqual(entry["host"], "claude")
        self.assertIsNotNone(entry["turn"])
        self.assertEqual(output, entry["verdict"] + "\n")

    def test_codex_ts_decide_returns_literal_verdict_stdout(self):
        payload = {**self.payload(""), "tool_name": "exec_command",
                   "tool_input": {"cmd": f"ts-decide {shlex.quote(json.dumps(REQUEST))}"},
                   "transcript_path": str(write_jsonl(self.dir, "codex.jsonl", [codex_user("Which cache?")]))}
        out = hook.handle("pre_tool_use", payload, "codex", post=FakeJev(**CONFIDENT))
        specific = out["hookSpecificOutput"]
        self.assertEqual(specific["permissionDecision"], "allow")
        self.assertIn("ts-decide verdict", self.verdict_output(out, "cmd"))

    def test_misuse_and_invalid_requests_are_denied_with_usage(self):
        for command in ("ts-decide '{}' | cat", "ts-decide 'not json'"):
            with self.subTest(command=command):
                out = hook.handle("pre_tool_use", self.payload(command), "claude", post=FakeJev())
                self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
                self.assertIn("usage: ts-decide", out["hookSpecificOutput"]["permissionDecisionReason"])

    def test_malformed_answer_and_missing_transcript_give_unverified_verdicts(self):
        command = f"ts-decide {shlex.quote(json.dumps(REQUEST))}"
        cases = [(self.payload(command), FakeJev(raw=b'{"answers": {}}'), "jev malformed"),
                 (self.payload(command, transcript_path=str(self.dir / "missing.jsonl")), FakeJev(),
                  "transcript unreadable")]
        for payload, fake, reason in cases:
            with self.subTest(reason=reason):
                out = hook.handle("pre_tool_use", payload, "claude", post=fake)
                output = self.verdict_output(out)
                self.assertIn("decision unverified", output)
                self.assertIn(reason, output)

    def test_guard_denies_and_records(self):
        out = hook.handle("pre_tool_use", self.payload(f"cat {policy.state_dir()}/sessions/s1.jsonl"),
                          "claude", post=FakeJev())
        self.assertEqual(out["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(ledger.entries("s1")[0]["gate"], "guard")

    def test_ordinary_tool_calls_pass_silently(self):
        self.assertIsNone(hook.handle("pre_tool_use", self.payload("ls"), "claude", post=FakeJev()))
        read = {**self.payload("ls"), "tool_name": "Read", "tool_input": {"file_path": "/x"}}
        self.assertIsNone(hook.handle("pre_tool_use", read, "claude", post=FakeJev()))

    def test_stop_block_shape_and_fail_open_note(self):
        payload = {"session_id": "s1", "transcript_path": str(self.transcript), "stop_hook_active": False,
                   "last_assistant_message": "I chose LRU over LFU."}
        out = hook.handle("stop", payload, "claude", post=FakeJev(made_choice=0.9))
        self.assertEqual(out["decision"], "block")
        self.assertIn("without a relevant ts-decide", out["reason"])
        out = hook.handle("stop", {**payload, "transcript_path": str(self.dir / "gone.jsonl")}, "claude", post=FakeJev())
        self.assertIn("systemMessage", out)

    def test_subagent_reads_its_own_transcript_and_the_users_words(self):
        write_jsonl(self.dir / "s1" / "subagents", "agent-a1.jsonl", [
            {"type": "user", "isSidechain": True, "message": {"role": "user", "content": "brief: pick a cache"}}])
        fake = FakeJev(**CONFIDENT)
        out = hook.handle("pre_tool_use", self.payload(f"ts-decide {shlex.quote(json.dumps(REQUEST))}",
                                                       agent_id="a1"), "claude", post=fake)
        self.assertIn("BINDING", self.verdict_output(out))
        evidence = fake.requests[0]["state"]["evidence"]
        self.assertEqual(evidence["brief"], "brief: pick a cache")
        self.assertEqual(evidence["user_messages"], ["speed up the tile cache"])

    def test_session_start_prunes(self):
        ledger.write_verdict("old", "v")
        old = 0
        os.utime(policy.state_dir() / "verdicts" / "old.txt", (old, old))
        out = hook.handle("session_start", {"session_id": "s1"}, "claude")
        self.assertEqual(out["hookSpecificOutput"]["hookEventName"], "SessionStart")
        self.assertIn(str(ROOT / "bin/ts-decide"), out["hookSpecificOutput"]["additionalContext"])
        self.assertFalse((policy.state_dir() / "verdicts" / "old.txt").exists())


class ScriptTest(StateTestCase):
    def run_script(self, args, stdin, **env):
        return subprocess.run([sys.executable, "-B", str(ROOT / "hooks" / "hook.py"), *args], input=stdin,
                              capture_output=True, text=True, env={**os.environ, **env}, timeout=30)

    def test_garbage_input_fails_open_with_a_note(self):
        result = self.run_script(["pre_tool_use", "--host", "claude"], "not json")
        self.assertEqual(result.returncode, 0)
        self.assertIn("agent-decisions: hook error", json.loads(result.stdout)["systemMessage"])

    def test_off_switch_produces_no_output(self):
        result = self.run_script(["pre_tool_use", "--host", "claude"], "not json", AGENT_DECISIONS_OFF="1")
        self.assertEqual((result.returncode, result.stdout), (0, ""))

    def test_stub_reports_an_inactive_hook(self):
        result = subprocess.run([str(ROOT / "bin" / "ts-decide"), "{}"], capture_output=True, text=True)
        self.assertEqual(result.returncode, 3)
        self.assertIn("agent-decisions hook inactive: decision unverified", result.stdout + result.stderr)
