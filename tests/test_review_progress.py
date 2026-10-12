from __future__ import annotations

import json

from agent_decisions import gates
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx


REQUEST = {"decision": "Which cache policy should the parser use?", "options": [
    {"id": "lru", "description": "Least recently used eviction"},
    {"id": "lfu", "description": "Least frequently used eviction"}],
    "scope": {"paths": ["parser.py"], "invariants": ["Retain each active input lease until parsing ends."]}}


def answers(*, tie=False, invariant_lfu=.95):
    base = asker(FakeJev(pick="lfu", sufficient=.95, owner="agent",
                        invariant_0_lru=.95, invariant_0_lfu=invariant_lfu))

    def ask(state, questions):
        result = base(state, questions)
        if tie:
            result["pick"] = {"type": "choice", "choice": "lfu", "confidence": .15,
                              "probabilities": {"lru": .42, "lfu": .43, "none_fit": .15}}
        return result
    return ask


class ReviewProgressTest(StateTestCase):
    def test_neutral_repair_of_an_argued_question_keeps_identity_and_rechecks(self):
        first = gates.decide(json.dumps(REQUEST), make_ctx(), asker(FakeJev(argued=.95, argued_field="decision")))
        self.assertEqual(first.action, "refused")
        entry = {**first.record, "host": "claude", "agent_id": None, "action": first.action, "verdict": first.message}
        repaired = {**REQUEST, "decision": "How should parser cache entries be evicted?"}
        second = gates.decide(json.dumps(repaired), make_ctx(entries=(entry,)), answers())
        self.assertEqual(second.action, "advisory")
        self.assertEqual(second.record["decision_key"], first.record["decision_key"])
        self.assertIn("repair field: decision", first.message)

    def test_large_invariant_cross_product_is_rejected_before_api_work(self):
        request = {**REQUEST, "options": [{"id": f"option_{i}", "description": f"Policy {i}"} for i in range(9)],
                   "scope": {"paths": ["parser.py"], "invariants": [f"Preserve lease {i}." for i in range(32)]}}
        fake = FakeJev()
        result = gates.decide(json.dumps(request), make_ctx(), asker(fake))
        self.assertEqual(result.action, "invalid")
        self.assertIn("256 invariant checks", result.message)
        self.assertEqual(fake.requests, [])
    def test_scoped_pick_violating_an_invariant_is_refused(self):
        result = gates.decide(json.dumps(REQUEST), make_ctx(), answers(invariant_lfu=.01))
        self.assertEqual(result.action, "refused")
        self.assertIn("invariant_0_lfu", result.message)

    def test_worker_near_tie_requires_parent_review_without_a_receipt(self):
        result = gates.decide(json.dumps(REQUEST), make_ctx(agent_id="worker"), answers(tie=True))
        self.assertEqual(result.action, "parent_review")
        self.assertNotIn("receipt_id", result.record)

    def test_main_near_tie_is_advisory_and_reports_uncertain_preference(self):
        result = gates.decide(json.dumps(REQUEST), make_ctx(), answers(tie=True))
        self.assertEqual(result.action, "advisory")
        self.assertIn("preference: uncertain", result.message)
        self.assertEqual(result.record["eligible_options"], ["lru", "lfu"])

    def test_scoped_identity_survives_a_neutral_question_repair(self):
        first = gates.decide(json.dumps(REQUEST), make_ctx(), answers())
        repaired = {**REQUEST, "decision": "How should parser cache entries be evicted?"}
        second = gates.decide(json.dumps(repaired), make_ctx(), answers())
        self.assertEqual(first.record["decision_key"], second.record["decision_key"])

    def test_question_rephrasing_cannot_reroll_an_unchanged_scoped_review(self):
        fake = FakeJev(pick="lfu", sufficient=.95, invariant_0_lru=.95, invariant_0_lfu=.95)
        first = gates.decide(json.dumps(REQUEST), make_ctx(), asker(fake))
        entry = {**first.record, "gate": "decide", "host": "claude", "agent_id": None,
                 "action": first.action, "verdict": first.message}
        repaired = {**REQUEST, "decision": "How should parser cache entries be evicted?"}
        second = gates.decide(json.dumps(repaired), make_ctx(entries=(entry,)), asker(fake))
        self.assertEqual(second.action, "advisory")
        self.assertEqual(len(fake.requests), 1)
        self.assertIn(first.record["receipt_id"], second.message)

    def test_new_observed_source_requires_a_fresh_review(self):
        fake = FakeJev(pick="lfu", sufficient=.95, invariant_0_lru=.95, invariant_0_lfu=.95)
        first = gates.decide(json.dumps(REQUEST), make_ctx(), asker(fake))
        entry = {**first.record, "gate": "decide", "host": "claude", "agent_id": None,
                 "action": first.action, "verdict": first.message}
        current = make_ctx(entries=(entry,))
        current.evidence["source_files"] = [{"path": "parser.py", "sha256": "new-content", "text": "changed"}]
        gates.decide(json.dumps(REQUEST), current, asker(fake))
        self.assertEqual(len(fake.requests), 2)

    def test_missing_implementation_check_names_the_observed_helper_path(self):
        context = make_ctx()
        context.evidence["source_files"] = [{"path": "src/owner.py", "sha256": "observed", "text": "owner code"}]
        result = gates.decide(json.dumps(REQUEST), context, asker(FakeJev(
            pick="lfu", sufficient=.95, unsupported=.95, evidence_needed="implementation",
            invariant_0_lru=.95, invariant_0_lfu=.95)))
        self.assertEqual(result.action, "gather_evidence")
        self.assertIn("src/owner.py", result.message)
