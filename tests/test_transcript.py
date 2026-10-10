from __future__ import annotations

import json
import time

from agent_decisions import transcript
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import (
    StateTestCase, assistant_text, codex_assistant, codex_call, codex_custom, codex_output,
    codex_user, human, peer, sidechain_user, tool_result, tool_use, write_jsonl)


class ClaudeTranscriptTest(StateTestCase):
    def test_reads_user_words_tool_calls_and_turn(self):
        path = write_jsonl(self.dir, "s.jsonl", [
            human("first ask"),
            tool_use("toolu_1", "Bash", {"command": "ls"}),
            tool_result("toolu_1", "a.txt"),
            peer("peer note"),
            {"type": "user", "isCompactSummary": True, "origin": {"kind": "human"},
             "message": {"role": "user", "content": "summary of earlier work"}},
            human("second ask"),
            tool_use("toolu_2", "Read", {"file_path": "/x"}),
            tool_result("toolu_2", [{"type": "text", "text": "line 1"}]),
            assistant_text("done"),
        ])
        t = transcript.load(path, "claude")
        self.assertEqual(t.user_messages, ("first ask", "second ask"))
        self.assertEqual([c.id for c in t.tool_calls], ["toolu_1", "toolu_2"])
        self.assertEqual(json.loads(t.tool_calls[0].input), {"command": "ls"})
        self.assertEqual(t.tool_calls[0].output, "a.txt")
        self.assertEqual(t.tool_calls[1].output, "line 1")
        self.assertEqual([c.id for c in t.turn_calls], ["toolu_2"])
        self.assertEqual(t.last_assistant_text, "done")
        self.assertIsNone(t.brief)

    def test_partial_last_line_is_skipped(self):
        path = write_jsonl(self.dir, "s.jsonl", [human("ask")], tail='{"type": "user", "mess')
        self.assertEqual(transcript.load(path, "claude").user_messages, ("ask",))

    def test_headless_prompt_without_origin_is_the_users(self):
        # `claude -p` writes the prompt with no `origin` (observed 2026-10-10).
        headless = {"type": "user", "isSidechain": False, "message": {"role": "user", "content": "probe ask"}}
        meta = {**headless, "isMeta": True, "message": {"role": "user", "content": "skill text"}}
        echo = {**headless, "message": {"role": "user", "content": "<bash-stdout>x</bash-stdout>"}}
        path = write_jsonl(self.dir, "s.jsonl", [headless, meta, echo])
        self.assertEqual(transcript.load(path, "claude").user_messages, ("probe ask",))

    def test_subagent_brief_starts_the_turn(self):
        path = write_jsonl(self.dir, "agent-a1.jsonl", [
            sidechain_user("Find why tiles reload"),
            {**tool_use("toolu_3", "Grep", {"pattern": "reload"}), "isSidechain": True},
            {**tool_result("toolu_3", "cache.py:10"), "isSidechain": True},
        ])
        t = transcript.load(path, "claude")
        self.assertEqual(t.brief, "Find why tiles reload")
        self.assertEqual(t.user_messages, ())
        self.assertEqual([c.id for c in t.turn_calls], ["toolu_3"])

    def test_turn_key_changes_only_with_a_new_user_message(self):
        a = Transcript(("one",), (), ())
        b = Transcript(("one",), (ToolCall("x", "Bash", "ls"),), ())
        c = Transcript(("one", "two"), (), ())
        self.assertEqual(a.turn_key, b.turn_key)
        self.assertNotEqual(a.turn_key, c.turn_key)

    def test_large_transcript_loads_in_under_two_seconds(self):
        records = []
        for i in range(25_000):
            records.append(tool_use(f"toolu_{i}", "Bash", {"command": f"echo {i}"}))
            records.append(tool_result(f"toolu_{i}", "x" * 200))
        path = write_jsonl(self.dir, "big.jsonl", [human("go"), *records])
        start = time.monotonic()
        t = transcript.load(path, "claude")
        self.assertLess(time.monotonic() - start, 2.0)
        self.assertEqual(len(t.tool_calls), 25_000)

    def test_is_decide_matches_original_and_rewritten_commands(self):
        self.assertTrue(transcript.is_decide(ToolCall("a", "Bash", '{"command": "ts-decide \'{}\'"}')))
        self.assertTrue(transcript.is_decide(ToolCall("b", "Bash", '{"command": "cat /s/verdicts/b.txt"}')))
        self.assertFalse(transcript.is_decide(ToolCall("c", "Bash", '{"command": "ls"}')))


class CodexTranscriptTest(StateTestCase):
    def test_code_mode_completed_events_preserve_hook_ids_and_outputs(self):
        records = [codex_user("Fix the parser."), codex_custom("outer", "exec", 'text(await tools.exec_command({cmd:"read"}));'),
                   codex_output("outer", "agent printed an invented result", "custom_tool_call_output"),
                   {"type": "event_msg", "payload": {"type": "item_completed", "item": {
                       "type": "CommandExecution", "id": "exec-observed", "command": ["/bin/zsh", "-c", "cat parser.py"],
                       "aggregated_output": "input.read_all()", "status": "completed"}}},
                   {"type": "event_msg", "payload": {"type": "item_completed", "item": {
                       "type": "McpToolCall", "id": "mcp-observed", "server": "docs", "tool": "read",
                       "arguments": {"url": "https://example.com"}, "result": {"content": [{"type": "text", "text": "official docs"}]}}}}]
        t = transcript.load(write_jsonl(self.dir, "code.jsonl", records), "codex")
        self.assertEqual([c.id for c in t.tool_calls], ["exec-observed", "mcp-observed"])
        self.assertEqual(t.tool_calls[0].output, "input.read_all()")
        self.assertIn("official docs", t.tool_calls[1].output)

    def test_code_mode_verdict_event_backs_consultation_and_is_excluded_from_evidence(self):
        from agent_decisions import evidence, gates, ledger, provenance
        from tests.helpers import make_ctx
        call_id = "exec-observed"
        command = f"cat '/state with spaces/verdicts/{ledger.verdict_name(call_id)}'"
        records = [codex_user("Fix the parser."), codex_custom("outer", "exec", 'text(await tools.exec_command({cmd:"ts-decide fabricated"}));'),
                   codex_output("outer", "fabricated consultation fact", "custom_tool_call_output"),
                   {"type": "event_msg", "payload": {"type": "item_completed", "item": {
                       "type": "CommandExecution", "id": call_id, "command": ["/bin/zsh", "-c", command],
                       "aggregated_output": "ts-decide verdict: ADVISORY\nfabricated consultation fact", "status": "completed"}}}]
        t = transcript.load(write_jsonl(self.dir, "code.jsonl", records), "codex")
        entry = {"gate": "decide", "action": "advisory", "tool_use_id": call_id}
        self.assertEqual(gates.consulted(make_ctx(t, entries=(entry,), host="codex")), [entry])
        self.assertEqual(evidence.build(t)["tool_calls_newest_first"], [])
        self.assertEqual(provenance.verify(["fabricated consultation fact"], t)[0], [])

    def test_duplicate_classic_and_event_calls_are_not_repeated(self):
        records = [codex_user("Read the parser."), codex_call("call1", "exec_command", '{"cmd":"cat parser.py"}'),
                   {"type": "event_msg", "payload": {"type": "item_completed", "item": {
                       "type": "CommandExecution", "id": "call1", "command": ["zsh", "-c", "cat parser.py"],
                       "aggregated_output": "source text"}}}]
        t = transcript.load(write_jsonl(self.dir, "direct.jsonl", records), "codex")
        self.assertEqual(len(t.tool_calls), 1)
        self.assertEqual(t.tool_calls[0].output, "source text")

    def test_skips_injected_context_and_matches_outputs(self):
        path = write_jsonl(self.dir, "rollout.jsonl", [
            codex_user("# AGENTS.md instructions for /x\n..."),
            codex_user("<environment_context>cwd</environment_context>"),
            codex_user("fix the build"),
            codex_call("call_1", "exec_command", '{"cmd": "make"}'),
            codex_output("call_1", "error: missing header"),
            codex_custom("call_2", "apply_patch", "*** Begin Patch"),
            codex_output("call_2", "Success", kind="custom_tool_call_output"),
            codex_assistant("patched"),
            {"type": "event_msg", "payload": {"type": "token_count"}},
        ])
        t = transcript.load(path, "codex")
        self.assertEqual(t.user_messages, ("fix the build",))
        self.assertEqual([(c.id, c.name, c.output) for c in t.tool_calls],
                         [("call_1", "exec_command", "error: missing header"), ("call_2", "apply_patch", "Success")])
        self.assertEqual(t.tool_calls[0].input, '{"cmd": "make"}')
        self.assertEqual(len(t.turn_calls), 2)
        self.assertEqual(t.last_assistant_text, "patched")
