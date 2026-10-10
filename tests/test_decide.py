from __future__ import annotations

import json

from agent_decisions import gates, jev, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx

REQUEST = {
    "decision": "Which eviction policy for the tile cache?",
    "options": [{"id": "lru", "description": "Least recently used"},
                {"id": "lfu", "description": "Least frequently used"}],
    "facts": ["tiles are re-read in bursts", "the user said cost does not matter"],
}
CONFIDENT = dict(pick="lfu", sufficient=0.9, missing_option=0.1, argued=0.1, owner="agent")


def run(fake, request=REQUEST, **ctx):
    t = Transcript(("decide the cache policy",),
                   (ToolCall("t1", "Bash", "trace", "Tiles are re-read in bursts while panning"),), ())
    return gates.decide(json.dumps(request), make_ctx(t, **ctx), asker(fake))


class DecideTest(StateTestCase):
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
                self.assertIn("refusal 1 of 2", outcome.message)

    def test_user_owned_decision_with_only_none_fit_asks_the_user(self):
        outcome = run(FakeJev(**{**CONFIDENT, "owner": "user", "pick": "none_fit"}))
        self.assertEqual(outcome.action, "ask_user")

    def test_user_owned_decision_with_a_missing_option_is_still_refused(self):
        outcome = run(FakeJev(**{**CONFIDENT, "owner": "user", "missing_option": 0.6}))
        self.assertEqual(outcome.action, "refused")

    def test_third_attempt_is_over_the_refusal_cap(self):
        key = gates.provenance.normalize(REQUEST["decision"])
        prior = [{"gate": "decide", "action": "refused", "decision_key": key}] * policy.REFUSAL_CAP
        outcome = run(FakeJev(**{**CONFIDENT, "argued": 0.9}), entries=prior)
        self.assertEqual(outcome.action, "advisory")
        self.assertIn("OVER REFUSAL CAP", outcome.message)

    def test_state_carries_verified_and_unverified_facts_and_policy(self):
        fake = FakeJev(**CONFIDENT)
        run(fake)
        state = fake.requests[0]["state"]
        self.assertEqual(state["facts"], ["tiles are re-read in bursts"])
        self.assertEqual(state["unverified_claims"], ["the user said cost does not matter"])
        self.assertEqual(state["policy"], policy.POLICY)
        self.assertIn("tool_calls_newest_first", state["evidence"])
        self.assertIn("none_fit", fake.requests[0]["questions"]["pick"]["criteria"])
        self.assertIn("the user said cost does not matter", run(FakeJev(**CONFIDENT)).message)

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
        turn = (ToolCall("toolu_a", "Bash", '{"command": "ts-decide \'{}\'"}'),
                ToolCall("toolu_b", "Bash", '{"command": "cat /x/verdicts/toolu_b.txt"}'),
                ToolCall("toolu_c", "Bash", '{"command": "ls"}'))
        entries = [{"gate": "decide", "tool_use_id": i, "action": "advisory"} for i in ("toolu_a", "toolu_b", "toolu_c", "toolu_forged")]
        ctx = make_ctx(Transcript(("go",), turn, turn), entries=entries)
        self.assertEqual([e["tool_use_id"] for e in gates.consulted(ctx)], ["toolu_a", "toolu_b"])
