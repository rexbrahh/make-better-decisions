from __future__ import annotations

import json

from agent_decisions import gates, jev
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx


class ToolCoverageTest(StateTestCase):
    def test_large_edit_preserves_existing_and_proposed_field_roles(self):
        tool_input = {"file_path": "parser.py", "old_string": "old line\n" * 2_000,
                      "new_string": "new line\n" * 2_000, "replace_all": False}
        parts = gates._pending_parts(tool_input)
        fields = [field for part in parts for field in part["fields"]]
        for key in ("old_string", "new_string"):
            self.assertEqual("".join(field["value"] for field in fields if field["path"] == [key]), tool_input[key])
        self.assertTrue(all(part["operation_context"]["file_path"] == "parser.py" for part in parts))
        self.assertTrue(all(len(json.dumps(part)) <= 16_000 for part in parts))

    def test_chunked_nested_multiedit_keeps_paths_and_boolean_values(self):
        data = {"file_path": "parser.py", "edits": [
            {"old_string": "x" * 20_000, "new_string": "y" * 20_000, "replace_all": False}]}
        fields = [field for part in gates._pending_parts(data) for field in part["fields"]]
        self.assertIn({"path": ["edits", 0, "replace_all"], "value": False}, fields)
        self.assertEqual("".join(field["value"] for field in fields
                                if field["path"] == ["edits", 0, "new_string"]), data["edits"][0]["new_string"])

    def test_large_write_cannot_hide_the_choice_between_the_head_and_tail(self):
        text = "# padding\n" * 2_000 + "read_chunks()" + "\n# padding" * 2_000
        fake = FakeJev(needs_decision=.99)
        out = gates.edit_gate("Write", {"file_path": "parser.py", "content": text}, make_ctx(), asker(fake))
        self.assertEqual(out.action, "deny")
        self.assertTrue(any("read_chunks()" in json.dumps(r["state"]["pending_input"]) for r in fake.requests))
        self.assertTrue(all(len(json.dumps(r["state"]["pending_input"])) < 20_000 for r in fake.requests))

    def test_chunk_focus_exposes_code_surrounded_by_comments(self):
        text = "# fixture\n" * 2_000 + "read_chunks()\n" + "# fixture\n" * 2_000
        parts = gates._pending_parts({"file_path": "parser.py", "content": text})
        self.assertTrue(any("read_chunks()" in part.get("proposed_excerpt", "") for part in parts))
        self.assertTrue(all(len(json.dumps(part)) <= 16_000 for part in parts))

    def test_interactive_code_cannot_mutate_files_through_unhooked_stdin(self):
        for command in ("python3", "bash -i", "node -i", "python3 -", "python3 -u -",
                        "bash -s", "bash -se", "sh -s", "node -", "python3 -m code", "python3 -mcode"):
            with self.subTest(command=command):
                out = gates.edit_gate("Bash", {"command": command}, make_ctx(), asker(FakeJev()))
                self.assertEqual(out.action, "deny")
                self.assertIn("stdin", out.message)

    def test_data_stdin_and_test_progress_are_allowed(self):
        for command in ("pytest -q", "cat", "python3 -c 'print(1)'", "npm test",
                        "python3 - <<'PY'\nprint(1)\nPY", "bash -s <<'SH'\nprintf ok\nSH",
                        "python3 - < inspect.py"):
            with self.subTest(command=command):
                out = gates.edit_gate("Bash", {"command": command, "tty": True}, make_ctx(), asker(FakeJev()))
                self.assertEqual(out.action, "allow")

    def test_quoted_redirect_token_does_not_authorize_future_code_stdin(self):
        out = gates.edit_gate("Bash", {"command": "python3 - '<<'"}, make_ctx(), asker(FakeJev()))
        self.assertEqual(out.action, "deny")

    def test_opaque_evaluator_is_denied_with_a_specific_repair(self):
        out = gates.edit_gate("Bash", {"command": "python3 -c 'exec(input())'"}, make_ctx(),
                              asker(FakeJev(accepts_code_input=.99)))
        self.assertEqual(out.action, "deny")
        self.assertIn("complete", out.message)

    def test_api_outage_still_allows_with_a_visible_note(self):
        out = gates.edit_gate("Bash", {"command": "python3"}, make_ctx(),
                              asker(FakeJev(raises=jev.JevError("timeout"))))
        self.assertEqual(out.action, "allow")
        self.assertIn("timeout", out.note)
