from __future__ import annotations

import json
from unittest import mock

from agent_decisions import gates, hook, jev, ledger, policy
from tests.helpers import FakeJev, StateTestCase, asker, human, make_ctx, write_jsonl


class MetricsTest(StateTestCase):
    def test_client_retains_usage_without_changing_model_state(self):
        questions = {"ok": {"type": "noul", "instructions": "Is this supported?"}}
        response = {"model": policy.MODEL, "answers": {"ok": {"type": "noul", "noul": .8}},
                    "usage": {"input_tokens": 120, "output_tokens": 4}}
        fake = FakeJev(raw=json.dumps(response).encode())
        metrics = {}
        jev.ask({"evidence": {}}, questions, key="k", post=fake, metrics=metrics)
        self.assertEqual(metrics["usage"], response["usage"])
        self.assertEqual(metrics["model"], policy.MODEL)
        self.assertGreater(metrics["request_bytes"], 0)
        self.assertGreaterEqual(metrics["api_ms"], 0)
        self.assertNotIn("metrics", fake.requests[0]["state"])

    def test_hook_records_context_and_api_metrics_outside_decision_state(self):
        path = write_jsonl(self.dir, "transcript.jsonl", [human("Fix the parser.")])
        payload = {"session_id": "s1", "tool_use_id": "edit", "tool_name": "Edit",
                   "tool_input": {"file_path": "parser.py", "new_string": "read_chunks()"},
                   "transcript_path": str(path), "cwd": str(self.dir)}
        fake = FakeJev()
        hook.handle("pre_tool_use", payload, "claude", post=fake)
        entry = next(row for row in ledger.entries("s1") if row.get("gate") == "edit")
        self.assertIn("transcript_ms", entry["metrics"])
        self.assertIn("evidence_ms", entry["metrics"])
        self.assertEqual(len(entry["metrics"]["api_calls"]), 1)
        self.assertNotIn("metrics", entry["state"])
        self.assertNotIn("metrics", fake.requests[0]["state"])

    def test_model_change_invalidates_identical_gate_cache(self):
        asked = {"questions": [{"question": "Which colour?", "options": []}]}
        first = gates.question_gate(asked, make_ctx(), asker(FakeJev(owner_0="user")))
        entry = {**first.record, "host": "claude", "agent_id": None, "action": first.action}
        fake = FakeJev(owner_0="user")
        with mock.patch.object(policy, "MODEL", "jev-next"):
            gates.question_gate(asked, make_ctx(entries=[entry]), asker(fake))
        self.assertEqual(len(fake.requests), 1)

    def test_operator_policy_change_invalidates_identical_gate_cache(self):
        asked = {"questions": [{"question": "Which colour?", "options": []}]}
        first = gates.question_gate(asked, make_ctx(), asker(FakeJev(owner_0="user")))
        entry = {**first.record, "host": "claude", "agent_id": None, "action": first.action}
        self.availability()
        fake = FakeJev(owner_0="user")
        gates.question_gate(asked, make_ctx(entries=[entry]), asker(fake))
        self.assertEqual(len(fake.requests), 1)
