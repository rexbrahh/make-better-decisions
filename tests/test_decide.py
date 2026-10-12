from __future__ import annotations

import json
from pathlib import Path

from agent_decisions import gates, jev, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx

REQUEST = {
    "decision": "Which eviction policy for the tile cache?",
    "options": [{"id": "lru", "description": "Least recently used"},
                {"id": "lfu", "description": "Least frequently used"}],
    "facts": ["decide the cache policy"],
}
CONFIDENT = dict(pick="lfu", sufficient=0.9, missing_option=0.1, argued=0.1, owner="agent")


def run(fake, request=REQUEST, **ctx):
    t = Transcript(("decide the cache policy",),
                   (ToolCall("t1", "Bash", "trace", "Tiles are re-read in bursts while panning"),), ())
    return gates.decide(json.dumps(request), make_ctx(t, **ctx), asker(fake))


class DecideTest(StateTestCase):
    def test_source_backed_main_agent_comparison_returns_advice_without_a_user_tiebreak(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .68, "pattern_present": .76,
                               "follows_pattern_lfu": .72}))
        self.assertEqual(result.action, "advisory")

    def test_advisory_ownership_does_not_need_worker_binding_certainty(self):
        def ask(state, questions):
            answers = asker(FakeJev(**{**CONFIDENT, "sufficient": .65}))(state, questions)
            answers["owner"]["probabilities"] = {"agent": .78, "user": .22}
            return answers

        self.assertEqual(gates.decide(json.dumps(REQUEST), make_ctx(), ask).action, "advisory")

    def test_observed_sdk_interface_pattern_supplies_a_selection_basis(self):
        request = json.loads((Path(__file__).parent / "fixtures" /
                              "sdk-save-export-requests.json").read_text())[-1]["request"]
        t = Transcript(("Continue implementing the SDK and preserve existing behavior.",), (), ())
        fake = FakeJev(pick="named_functions", sufficient=.52, unsupported=.57,
                       omitted_requirement=.38, missing_option=.26, argued=.16,
                       pattern_present=.87, follows_pattern_named_functions=.78)
        outcome = gates.decide(json.dumps(request), make_ctx(t), asker(fake))
        self.assertEqual(outcome.action, "advisory")

    def test_medium_pattern_match_is_not_binding_for_a_worker(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .52, "pattern_present": .87,
                               "follows_pattern_lfu": .78}), agent_id="worker")
        self.assertEqual(result.action, "gather_evidence")

    def test_medium_pattern_match_does_not_settle_uncertain_user_ownership(self):
        def ask(state, questions):
            answers = asker(FakeJev(**{**CONFIDENT, "sufficient": .52, "pattern_present": .87,
                                      "follows_pattern_lfu": .78}))(state, questions)
            answers["owner"]["probabilities"] = {"agent": .55, "user": .45}
            answers["owner"]["confidence"] = .1
            return answers

        t = Transcript(("decide the cache policy",), (), ())
        self.assertEqual(gates.decide(json.dumps(REQUEST), make_ctx(t), ask).action, "gather_evidence")

    def test_existing_pattern_cannot_ground_a_pick_that_does_not_follow_it(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .1, "pattern_present": .95,
                               "follows_pattern_lfu": .1}))
        self.assertEqual(result.action, "gather_evidence")

    def test_pattern_reuse_does_not_authorize_missing_required_facts_or_constraints(self):
        for missing in ("unsupported", "omitted_requirement"):
            with self.subTest(missing=missing):
                result = run(FakeJev(**{**CONFIDENT, "sufficient": .52, "pattern_present": .95,
                                       "follows_pattern_lfu": .96, missing: .95}))
                self.assertEqual(result.action, "gather_evidence")

    def test_existing_helper_omission_is_composed_from_independent_conditions(self):
        result = run(FakeJev(**{**CONFIDENT, "missing_option": .58,
                               "helper_present": .93, "helper_in_options": .1}))
        self.assertEqual(result.action, "refused")
        self.assertIn("missing_existing_helper", result.message)

    def test_exact_user_operation_does_not_require_reusing_an_existing_helper(self):
        result = run(FakeJev(**{**CONFIDENT, "specified_lfu": .95,
                               "helper_present": .93, "helper_in_options": .1}))
        self.assertEqual(result.action, "routine")

    def test_pick_cannot_contradict_the_option_prescribed_by_the_user(self):
        result = run(FakeJev(**{**CONFIDENT, "specified_lru": .95, "specified_lfu": .01}))
        self.assertEqual(result.action, "refused")
        self.assertIn("pick_conflicts_with_instruction", result.message)

    def test_no_fit_due_to_missing_evidence_requests_the_missing_evidence(self):
        result = run(FakeJev(**{**CONFIDENT, "pick": "none_fit", "sufficient": .1,
                               "unsupported": .95, "omitted_requirement": .95}))
        self.assertEqual(result.action, "gather_evidence")

    def test_omitted_requirement_is_checked_independently_of_unsupported_claim(self):
        result = run(FakeJev(**{**CONFIDENT, "unsupported": .01, "omitted_requirement": .95}))
        self.assertEqual(result.action, "gather_evidence")

    def test_routine_operation_does_not_require_an_exact_option_instruction(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .2, "specified_lfu": .01,
                               "routine_operation": .95}))
        self.assertEqual(result.action, "routine")
    def test_authorized_routine_work_does_not_wait_for_design_sufficiency(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .56, "specified_lfu": .95}))
        self.assertEqual(result.action, "routine")
        self.assertIn("PROCEED", result.message)

    def test_routine_label_cannot_authorize_unsupported_claims(self):
        result = run(FakeJev(**{**CONFIDENT, "sufficient": .56, "specified_lfu": .95, "unsupported": .95}))
        self.assertEqual(result.action, "gather_evidence")

    def test_main_agent_gets_an_advisory_verdict(self):
        outcome = run(FakeJev(**CONFIDENT))
        self.assertEqual(outcome.action, "advisory")
        self.assertIn("ts-decide verdict: ADVISORY", outcome.message)
        self.assertIn("pick: lfu", outcome.message)
        self.assertEqual(outcome.record["pick"], "lfu")

    def test_subagent_with_sufficient_evidence_gets_a_binding_verdict(self):
        self.assertEqual(run(FakeJev(**CONFIDENT), agent_id="a1").action, "binding")

    def test_insufficient_evidence_is_never_binding(self):
        outcome = run(FakeJev(**{**CONFIDENT, "sufficient": 0.79}), agent_id="a1")
        self.assertEqual(outcome.action, "gather_evidence")
        self.assertIn("Gather it with tool calls", outcome.message)

    def test_user_owned_decision_sends_the_agent_to_the_user(self):
        self.assertEqual(run(FakeJev(**{**CONFIDENT, "owner": "user"}), agent_id="a1").action, "ask_user")

    def test_missing_option_argued_and_none_fit_refuse(self):
        for override in ({"missing_option": 0.6}, {"argued": 0.6}, {"pick": "none_fit"}):
            with self.subTest(override):
                outcome = run(FakeJev(**{**CONFIDENT, **override}))
                self.assertEqual(outcome.action, "refused")
                self.assertIn("attempt 1", outcome.message)

    def test_user_owned_decision_with_only_none_fit_asks_the_user(self):
        outcome = run(FakeJev(**{**CONFIDENT, "owner": "user", "pick": "none_fit"}))
        self.assertEqual(outcome.action, "ask_user")

    def test_user_owned_decision_with_a_missing_option_is_still_refused(self):
        outcome = run(FakeJev(**{**CONFIDENT, "owner": "user", "missing_option": 0.6}))
        self.assertEqual(outcome.action, "refused")

    def test_third_attempt_still_requires_repair(self):
        key = gates.provenance.normalize(REQUEST["decision"])
        prior = [{"gate": "decide", "action": "refused", "decision_key": key}] * policy.REFUSAL_CAP
        outcome = run(FakeJev(**{**CONFIDENT, "argued": 0.9}), entries=prior)
        self.assertEqual(outcome.action, "refused")
        self.assertIn("REQUEST STILL NEEDS REPAIR", outcome.message)

    def test_state_carries_verified_user_facts_and_policy(self):
        fake = FakeJev(**CONFIDENT)
        run(fake)
        state = fake.requests[0]["state"]
        self.assertEqual(state["facts"], REQUEST["facts"])
        self.assertEqual(state["unverified_claims"], [])
        self.assertEqual(state["policy"], policy.POLICY)
        self.assertIn("tool_calls_newest_first", state["evidence"])
        self.assertIn("none_fit", fake.requests[0]["questions"]["pick"]["criteria"])
        self.assertIn("decide the cache policy", state["facts"])

    def test_invalid_requests_are_rejected_without_a_jev_call(self):
        bad = ["not json", "[]", json.dumps({**REQUEST, "decision": " "}),
               json.dumps({**REQUEST, "options": REQUEST["options"][:1]}),
               json.dumps({**REQUEST, "options": [{"id": "none_fit", "description": "x"}, REQUEST["options"][0]]}),
               json.dumps({**REQUEST, "options": [REQUEST["options"][0], REQUEST["options"][0]]}),
               json.dumps({**REQUEST, "options": [{"id": "Bad Id", "description": "x"}, REQUEST["options"][0]]}),
               json.dumps({**REQUEST, "facts": "one fact"})]
        for raw in bad:
            with self.subTest(raw=raw[:40]):
                fake = FakeJev()
                outcome = gates.decide(raw, make_ctx(), asker(fake))
                self.assertEqual(outcome.action, "invalid")
                self.assertIn("usage: ts-decide", outcome.message)
                self.assertIsNone(outcome.record)
                self.assertEqual(fake.requests, [])

    def test_jev_failure_is_an_unverified_decision(self):
        outcome = run(FakeJev(raises=jev.JevError("timeout")))
        self.assertEqual(outcome.action, "unverified")
        self.assertIn("decision unverified (jev timeout)", outcome.message)
        self.assertEqual(outcome.record["gate"], "decide")


class ConsultedTest(StateTestCase):
    def test_only_ledger_entries_backed_by_a_ts_decide_call_this_turn_count(self):
        turn = (ToolCall("toolu_a", "Bash", '{"command": "ts-decide \'{}\'"}', "ts-decide verdict: ADVISORY"),
                ToolCall("toolu_b", "Bash", '{"command": "cat /x/verdicts/toolu_b.txt"}', "ts-decide verdict: ADVISORY"),
                ToolCall("toolu_c", "Bash", '{"command": "ls"}'))
        entries = [{"gate": "decide", "tool_use_id": i, "action": "advisory"} for i in ("toolu_a", "toolu_b", "toolu_c", "toolu_forged")]
        ctx = make_ctx(Transcript(("go",), turn, turn), entries=entries)
        self.assertEqual([e["tool_use_id"] for e in gates.consulted(ctx)], ["toolu_a", "toolu_b"])
