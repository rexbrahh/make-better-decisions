from __future__ import annotations

import json
import io
from contextlib import redirect_stderr
from unittest import mock

from agent_decisions import hook, install, ledger, policy
from tests.helpers import FakeJev, StateTestCase, codex_user, human, write_jsonl
from tests.test_receipts import native_metadata


class SessionExceptionTest(StateTestCase):
    def test_exception_flags_require_explicit_codex_host(self):
        for host in ("both", "claude"):
            with self.subTest(host=host), redirect_stderr(io.StringIO()), \
                    mock.patch.object(install, "read_codex_servers", side_effect=AssertionError("unexpected discovery")), \
                    mock.patch.object(install, "read_json", side_effect=AssertionError("unexpected owner config read")):
                with self.assertRaises(SystemExit) as rejected:
                    install.cli(["--host", host, "--session-review-exception", "approved-sdk"])
                self.assertEqual(rejected.exception.code, 2)

    def test_codex_exception_does_not_create_a_claude_bypass(self):
        entry = install.installation_entry("claude", self.dir / "state", {}, [], None, [])
        entry["spec"]["session_review_exceptions"] = ["approved-sdk"]
        ledger.append(entry)
        path = write_jsonl(self.dir, "claude.jsonl", [human("Continue authorized work.")])
        api = FakeJev(needs_decision=1.0)
        result = hook.handle("pre_tool_use", {"session_id": "approved-sdk", "tool_name": "Edit",
            "tool_input": {"file_path": "parser.py"}, "transcript_path": str(path), "cwd": str(self.dir)}, "claude", post=api)
        self.assertEqual(result["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(len(api.requests), 1)

    def configure(self, sessions):
        entry = install.installation_entry("codex", self.dir / "state", {}, [], None, [])
        entry["spec"]["session_review_exceptions"] = sessions
        ledger.append(entry)

    def payload(self, native_session="approved-sdk", *, claimed_session=None, child=False):
        path = write_jsonl(self.dir, "native.jsonl", [
            native_metadata("child" if child else native_session, native_session, self.dir,
                            parent=native_session if child else None, agent="/root/sdk" if child else None),
            codex_user("Continue the authorized SDK fixes.")])
        return {"session_id": claimed_session or native_session, "tool_name": "apply_patch",
                "tool_use_id": "edit", "tool_input": {"input": "*** Begin Patch\n*** End Patch"},
                "transcript_path": str(path), "cwd": str(self.dir)}

    def test_signed_exception_honors_owner_even_when_semantic_review_would_deny(self):
        self.configure(["approved-sdk"])
        api = FakeJev(needs_decision=1.0)
        self.assertIsNone(hook.handle("pre_tool_use", self.payload(), "codex", post=api))
        self.assertEqual(api.requests, [])

    def test_exception_uses_native_identity_and_applies_to_native_workers(self):
        self.configure(["approved-sdk"])
        for child in (False, True):
            api = FakeJev(needs_decision=1.0)
            self.assertIsNone(hook.handle("pre_tool_use", self.payload(child=child), "codex", post=api))
            self.assertEqual(api.requests, [])
        api = FakeJev(needs_decision=1.0)
        result = hook.handle("pre_tool_use", self.payload("other-session", claimed_session="approved-sdk"), "codex", post=api)
        self.assertEqual(result["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(len(api.requests), 1)

    def test_private_guard_precedes_exception_and_revocation_restores_required_review(self):
        self.configure(["approved-sdk"])
        payload = self.payload()
        payload["tool_input"] = {"file_path": str(self.dir / "state/key")}
        api = FakeJev(needs_decision=1.0)
        self.assertEqual(hook.handle("pre_tool_use", payload, "codex", post=api)["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertEqual(api.requests, [])
        self.configure([])
        result = hook.handle("pre_tool_use", self.payload(), "codex", post=api)
        self.assertEqual(result["hookSpecificOutput"]["permissionDecision"], "deny")

    def test_tampered_owner_policy_cannot_extend_exception(self):
        self.configure(["approved-sdk"])
        path = self.dir / "state/sessions/installation.jsonl"
        row = json.loads(path.read_text().splitlines()[0])
        row["entry"]["spec"]["session_review_exceptions"] = ["other-session"]
        with path.open("a") as stream:
            stream.write(json.dumps(row) + "\n")
        self.assertFalse(policy.session_review_exception("codex", "other-session"))
        self.assertTrue(policy.session_review_exception("codex", "approved-sdk"))

    def test_installer_preserves_and_explicitly_revokes_exception(self):
        prior = {"session_review_exceptions": ["approved-sdk"]}
        entry = install.installation_entry("codex", self.dir / "state", {}, [], None, [])
        install.configure_session_exceptions(entry, prior)
        self.assertEqual(entry["spec"]["session_review_exceptions"], ["approved-sdk"])
        install.configure_session_exceptions(entry, prior, add=["other"], remove=["approved-sdk"])
        self.assertEqual(entry["spec"]["session_review_exceptions"], ["other"])
