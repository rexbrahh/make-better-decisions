from __future__ import annotations

import json
from unittest import mock

from agent_decisions import gates, policy
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
    def test_previous_question_contract_cannot_reuse_a_scoped_gather(self):
        fake = FakeJev(pick="lfu", sufficient=.83, unsupported=.82,
                       missing_fact="not_identified", invariant_0_lru=.60, invariant_0_lfu=.40)
        with mock.patch.object(policy, "REVIEW_CONTRACT", "scoped-review-1"):
            first = gates.decide(json.dumps(REQUEST), make_ctx(), asker(fake))
        entry = {**first.record, "host": "claude", "agent_id": None,
                 "action": first.action, "verdict": first.message}
        second = gates.decide(json.dumps(REQUEST), make_ctx(entries=(entry,)), asker(fake))
        self.assertEqual(len(fake.requests), 2)
        self.assertNotIn("reused:", second.message)
        self.assertNotEqual(first.record["evidence_fingerprint"], second.record["evidence_fingerprint"])

    def test_peer_unidentified_gather_names_invariants_without_repeating_comparison(self):
        # bbce7c returned these predicates after the peer completed the named comparison.
        request = {"decision": "Which isolated Windows build route should capture the trim refusal cause first?",
                   "options": [{"id": "single_tu_diagnostic_relink", "description": "Prepare a private diagnostic relink."},
                               {"id": "fresh_isolated_complete_build", "description": "Prepare a fresh isolated build."}],
                   "scope": {"paths": ["app/shell/"], "invariants": [
                       "Preserve the retained source and build artifacts unchanged.",
                       "Keep all diagnostic outputs in a separate campaign root."]}}
        fake = FakeJev(pick="single_tu_diagnostic_relink", sufficient=.83, unsupported=.82,
                       missing_fact="not_identified", evidence_needed="comparison",
                       invariant_0_single_tu_diagnostic_relink=.40,
                       invariant_1_single_tu_diagnostic_relink=.60)
        result = gates.decide(json.dumps(request), make_ctx(), asker(fake))
        self.assertEqual(result.action, "gather_evidence")
        self.assertNotIn("receipt_id", result.record)
        for invariant in request["scope"]["invariants"]:
            self.assertIn(invariant, result.message)
        self.assertIn("Do not repeat a completed comparison", result.message)
        self.assertNotIn("Run a check comparing these options", result.message)

    def test_recorded_stage_comparison_preserves_negative_cases(self):
        # Native 8354e2: the clarified questions judged a proposed guard, an
        # explicit overwrite, and reliance on an uninspected existing helper.
        for unsupported, invariant, expected in ((.31, .80, "advisory"),
                                                 (.44, .05, "refused"),
                                                 (.82, .09, "refused")):
            with self.subTest(unsupported=unsupported, invariant=invariant):
                fake = FakeJev(pick="lfu", sufficient=.95, unsupported=unsupported,
                               invariant_0_lru=.95, invariant_0_lfu=invariant)
                result = gates.decide(json.dumps(REQUEST), make_ctx(), asker(fake))
                self.assertEqual(result.action, expected)
                if expected != "advisory":
                    self.assertNotIn("receipt_id", result.record)

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
        self.assertIn(REQUEST["scope"]["invariants"][0], result.message)

    def test_each_refusal_reason_names_its_repair(self):
        # Native 2026-10-14 02:23Z: a missing_existing_helper refusal printed only the generic next step.
        cases = (({"missing_option": .9}, "missing_option", "Add the alternative"),
                 ({"helper_present": .9}, "missing_existing_helper", "helper_present 0.90"),
                 ({"specified_lru": .9}, "pick_conflicts_with_instruction", "prescribe lru"),
                 ({"pick": "none_fit"}, "none_fit", "Add the option that the evidence supports"))
        for overrides, reason, repair in cases:
            with self.subTest(reason=reason):
                fake = FakeJev(**{"pick": "lfu", "sufficient": .95, "owner": "agent", **overrides})
                result = gates.decide(json.dumps({"decision": REQUEST["decision"], "options": REQUEST["options"]}),
                                      make_ctx(), asker(fake))
                self.assertEqual(result.action, "refused")
                self.assertIn(f"Refused ({reason})", result.message)
                self.assertIn("repair: ", result.message)
                self.assertIn(repair, result.message)

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
