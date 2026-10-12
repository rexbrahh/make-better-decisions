from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

from agent_decisions import gates, hook, jev, ledger
from tests.helpers import FakeJev, StateTestCase, asker, human, make_ctx, write_jsonl


ROOT = Path(__file__).resolve().parent.parent


class RequiredReviewTest(StateTestCase):
    def test_timeout_cannot_authorize_an_unchecked_write_by_default(self):
        result = gates.edit_gate("Edit", {"file_path": "parser.py", "new_string": "read_chunks()"},
                                 make_ctx(), asker(FakeJev(raises=jev.JevError("timeout"))))
        self.assertEqual(result.action, "deny")
        self.assertIn("required", result.message.lower())

    def test_signed_operator_availability_policy_retains_visible_fail_open(self):
        ledger.append({"session_id": "installation", "gate": "installation", "action": "configured",
                       "spec": {"host": "claude", "review_mode": "availability"}})
        result = gates.edit_gate("Edit", {"file_path": "parser.py", "new_string": "read_chunks()"},
                                 make_ctx(), asker(FakeJev(raises=jev.JevError("timeout"))))
        self.assertEqual(result.action, "allow")
        self.assertIn("action allowed", result.note)

    def test_unreadable_context_blocks_an_unchecked_write(self):
        result = hook.handle("pre_tool_use", {"session_id": "s1", "tool_use_id": "edit1",
            "tool_name": "Edit", "tool_input": {"file_path": "parser.py", "new_string": "read_chunks()"},
            "transcript_path": str(self.dir / "missing.jsonl")}, "claude", post=FakeJev())
        self.assertEqual(result.get("hookSpecificOutput", {}).get("permissionDecision"), "deny")

    def test_verified_read_tools_continue_without_a_context_or_api(self):
        result = hook.handle("pre_tool_use", {"session_id": "s1", "tool_use_id": "read1",
            "tool_name": "Read", "tool_input": {"file_path": "parser.py"},
            "transcript_path": str(self.dir / "missing.jsonl")}, "claude", post=FakeJev(raises=jev.JevError("timeout")))
        self.assertIsNone(result)

    def test_environment_off_flag_cannot_silently_disable_the_hook(self):
        payload = {"session_id": "s1", "tool_use_id": "edit1", "tool_name": "Edit",
                   "tool_input": {"file_path": "parser.py", "new_string": "read_chunks()"},
                   "transcript_path": str(self.dir / "missing.jsonl")}
        result = subprocess.run([sys.executable, "-I", "-B", str(ROOT / "hooks/hook.py"),
                                 "pre_tool_use", "--host", "claude"], input=json.dumps(payload),
                                capture_output=True, text=True, timeout=10,
                                env={**os.environ, "AGENT_DECISIONS_OFF": "1"})
        response = json.loads(result.stdout) if result.stdout else {}
        self.assertEqual(response.get("hookSpecificOutput", {}).get("permissionDecision"), "deny")
