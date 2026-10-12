from __future__ import annotations

import hashlib
import json
import os
import subprocess
import time

from agent_decisions import evidence, gates, hook, ledger, policy, transcript
from tests.helpers import FakeJev, StateTestCase, codex_call, codex_output, codex_user, write_jsonl


class ContinuityTest(StateTestCase):
    def setUp(self):
        super().setUp()
        self.contract = self.dir / "contract.md"
        self.contract.write_text("Retain active leases until work ends.\n")
        self.path = write_jsonl(self.dir, "rollout.jsonl", [codex_user("Implement the lease contract.")])

    def payload(self, name="Edit", value=None, session="s1"):
        return {"session_id": session, "tool_use_id": "current", "tool_name": name,
                "tool_input": value or {"file_path": "parser.py"}, "transcript_path": str(self.path),
                "cwd": str(self.dir)}

    def completed_review(self, *, contracts=True):
        request = {"decision": "Which lease representation should parsing use?", "options": [
            {"id": "shared", "description": "Reference-counted lease"},
            {"id": "owned", "description": "Single-owner lease"}],
            "scope": {"paths": ["parser.py"], "invariants": ["Retain active leases until work ends."]},
            "contracts": ["contract.md"] if contracts else []}
        value = {"command": "ts-decide <<'EOF'\n" + json.dumps(request) + "\nEOF"}
        payload = {**self.payload("Bash", value), "tool_use_id": "review"}
        result = hook.handle("pre_tool_use", payload, "codex", post=FakeJev(sufficient=.95,
            owner="agent", invariant_0_shared=.95, invariant_0_owned=.95))
        command = result["hookSpecificOutput"]["updatedInput"]["command"]
        rendered = subprocess.run(["/bin/sh", "-c", command], capture_output=True, text=True, check=True).stdout
        write_jsonl(self.dir, "rollout.jsonl", [codex_user("Implement the lease contract."),
            codex_call("review", "Bash", json.dumps(value)), codex_output("review", rendered)])
        ctx = hook._context(self.payload(), "codex")
        return ctx

    def test_contract_hash_is_captured_from_the_current_file(self):
        ctx = self.completed_review()
        review = next(entry for entry in ctx.entries if entry.get("gate") == "decide")
        self.assertEqual(review["contracts"], {"contract.md": hashlib.sha256(self.contract.read_bytes()).hexdigest()})

    def test_completed_contract_review_becomes_durable_background_evidence(self):
        self.completed_review()
        path = ledger.architecture_path(hook._repository_id(str(self.dir)))
        old = time.time() - (policy.RETAIN_DAYS + 1) * 86_400
        os.utime(path, (old, old))
        ledger.prune()
        ctx = hook._context(self.payload(session="fresh"), "codex")
        history = ctx.evidence["architecture_history"]
        self.assertEqual(history[0]["contract_status"]["contract.md"], "unchanged")
        self.assertFalse(history[0]["authorizes_write"])
        self.assertEqual(gates.consulted(ctx), [])
        denied = gates.edit_gate("Edit", {"file_path": "parser.py"}, ctx,
                                lambda state, questions: {"writes_files": {"noul": 1.0},
                                "needs_decision": {"noul": 1.0}, "covered": {"noul": 1.0}})
        self.assertEqual(denied.action, "deny")

    def test_changed_contract_marks_history_stale_without_hiding_the_old_hash(self):
        self.completed_review()
        old_hash = hashlib.sha256(self.contract.read_bytes()).hexdigest()
        self.contract.write_text("Release leases after Flush.\n")
        ctx = hook._context(self.payload(session="fresh"), "codex")
        item = ctx.evidence["architecture_history"][0]
        self.assertEqual(item["contract_status"]["contract.md"], "changed")
        self.assertEqual(item["contracts"]["contract.md"], old_hash)

    def test_contract_history_is_repository_scoped(self):
        self.completed_review()
        other = self.dir / "other"
        other.mkdir()
        ctx = hook._context({**self.payload(session="fresh"), "cwd": str(other)}, "codex")
        self.assertEqual(ctx.evidence.get("architecture_history", []), [])

    def test_changed_contract_invalidates_the_current_approach_receipt(self):
        self.completed_review()
        self.contract.write_text("Release leases after Flush.\n")
        ctx = hook._context(self.payload(), "codex")
        self.assertEqual(gates._decision_summaries(ctx, json.dumps({"file_path": "parser.py"})), [])

    def test_changed_scoped_source_invalidates_coverage_without_declared_contracts(self):
        source = self.dir / "parser.py"
        source.write_text("def parse(owner): return owner.acquire()\n")
        ctx = self.completed_review(contracts=False)
        self.assertEqual(len(gates._decision_summaries(ctx, json.dumps({"file_path": "parser.py"}))), 1)
        source.write_text("def parse(owner): return None\n")
        ctx = hook._context(self.payload(), "codex")
        self.assertEqual(gates._decision_summaries(ctx, json.dumps({"file_path": "parser.py"})), [])
        self.assertEqual(gates.plan_gate(ctx).action, "deny")
        fake = FakeJev(made_choice=1.0, follows_pick=1.0)
        outcome = gates.stop_gate("I selected reference-counted leases.", False, ctx, hook._asker(fake))
        self.assertEqual(outcome.action, "block")
        self.assertEqual(fake.requests[0]["state"]["decisions"], [])

    def test_unavailable_scoped_source_is_ineligible_but_routine_work_still_uses_its_classifier(self):
        source = self.dir / "parser.py"
        source.write_text("def parse(owner): return owner.acquire()\n")
        self.completed_review(contracts=False)
        source.unlink()
        ctx = hook._context(self.payload(), "codex")
        self.assertEqual(gates._decision_summaries(ctx, json.dumps({"file_path": "parser.py"})), [])
        outcome = gates.edit_gate("Edit", {"file_path": "tests/test_parser.py"}, ctx,
                                 hook._asker(FakeJev(needs_decision=.01, writes_files=.99)))
        self.assertEqual(outcome.action, "allow")

    def test_missing_contract_cannot_create_a_receipt(self):
        request = {"decision": "Which lease representation?", "contracts": ["missing.md"], "options": [
            {"id": "shared", "description": "Reference-counted lease"},
            {"id": "owned", "description": "Single-owner lease"}]}
        ctx = hook._context(self.payload(), "codex")
        fake = FakeJev(sufficient=.95)
        result = gates.decide(json.dumps(request), ctx, hook._asker(fake))
        self.assertEqual(result.action, "gather_evidence")
        self.assertIn("missing.md", result.message)
        self.assertNotIn("receipt_id", result.record)
        self.assertEqual(fake.requests, [])

    def test_historical_records_are_signed_and_tampering_is_ignored(self):
        self.completed_review()
        path = ledger.architecture_path(hook._repository_id(str(self.dir)))
        rows = [json.loads(line) for line in path.read_text().splitlines()]
        rows[0]["entry"]["selected_approach"] = "Delete all leases"
        path.write_text(json.dumps(rows[0]) + "\n")
        ctx = hook._context(self.payload(session="fresh"), "codex")
        self.assertEqual(ctx.evidence.get("architecture_history", []), [])

    def test_native_command_retains_cwd_and_exit_status_as_observations(self):
        item = {"type": "CommandExecution", "id": "check", "command": ["/bin/sh", "-c", "printf 'tests passed'"],
                "cwd": "file:///tmp/repo", "status": "completed", "exit_code": 1,
                "aggregated_output": "tests passed"}
        path = write_jsonl(self.dir, "command.jsonl", [codex_user("Check the tests."),
                {"type": "event_msg", "payload": {"type": "item_completed", "item": item}}])
        bundle = evidence.build(transcript.load(path, "codex"))
        observation = bundle["tool_calls_newest_first"][0]
        self.assertEqual(observation["execution"]["exit_code"], 1)
        self.assertEqual(observation["execution"]["cwd"], item["cwd"])
        self.assertEqual(observation["trust"], "unverified_tool_observation")

    def test_current_source_is_distinguished_from_its_git_baseline(self):
        def git(*args):
            return subprocess.run(["git", "-C", str(self.dir), *args], capture_output=True, text=True, check=True).stdout.strip()
        git("init", "-q")
        git("add", "contract.md")
        git("-c", "user.name=Test", "-c", "user.email=test@example.test", "-c", "commit.gpgsign=false",
            "commit", "-qm", "fixture")
        revision = git("rev-parse", "HEAD")
        baseline_hash = hashlib.sha256(self.contract.read_bytes()).hexdigest()
        self.contract.write_text("Changed lease contract.\n")
        bundle = evidence.build(transcript.Transcript(("Inspect the lease contract.",), (), ()),
                                cwd=self.dir, query="contract.md")
        item = bundle["source_files"][0]
        self.assertEqual(item["role"], "working_tree_source")
        self.assertEqual(item["git_baseline"]["revision"], revision)
        self.assertEqual(item["git_baseline"]["sha256"], baseline_hash)
        self.assertTrue(item["git_baseline"]["differs_from_worktree"])
