from __future__ import annotations

from dataclasses import replace

from agent_decisions import gates, jev
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx

DECIDE = ToolCall("toolu_d", "Bash", '{"command": "ts-decide \'{}\'"}', "ts-decide verdict: ADVISORY")
BINDING = {"gate": "decide", "tool_use_id": "toolu_d", "action": "binding", "decision": "Which cache?",
           "pick": "lfu", "state": {"options": {"lru": "LRU", "lfu": "LFU"}}}


def ctx_with(calls=(), entries=(), host="claude"):
    calls = tuple(calls)
    return make_ctx(Transcript(("fix the cache",), calls, calls), entries=entries, host=host)


class StopGateTest(StateTestCase):
    def test_stop_hook_active_still_checks_a_new_choice(self):
        fake = FakeJev(made_choice=1.0)
        self.assertEqual(gates.stop_gate("I chose LRU.", True, ctx_with(), asker(fake)).action, "block")
        self.assertEqual(len(fake.requests), 1)

    def test_unconsulted_choice_blocks(self):
        outcome = gates.stop_gate("I chose LRU over LFU.", False, ctx_with(), asker(FakeJev(made_choice=0.7)))
        self.assertEqual(outcome.action, "block")
        self.assertIn("without a relevant ts-decide", outcome.message)

    def test_consulted_choice_passes(self):
        ctx = ctx_with([DECIDE], [{"gate": "decide", "tool_use_id": "toolu_d", "action": "advisory"}])
        self.assertEqual(gates.stop_gate("I chose LRU.", False, ctx, asker(FakeJev(made_choice=0.9, uncovered_choice=.05))).action, "allow")

    def test_an_additional_choice_blocks_even_when_other_choices_follow_bindings(self):
        ctx = ctx_with([DECIDE], [{"gate": "decide", "tool_use_id": DECIDE.id, "action": "advisory"}])
        outcome = gates.stop_gate("I used LRU and PostgreSQL.", False, ctx,
                                  asker(FakeJev(made_choice=.95, uncovered_choice=.76)))
        self.assertEqual(outcome.action, "block")

    def test_question_the_user_already_answered_blocks(self):
        outcome = gates.stop_gate("Should I use Postgres?", False, ctx_with(),
                                  asker(FakeJev(asks_user=0.8, stated=0.9, owner="user")))
        self.assertEqual(outcome.action, "block")
        self.assertIn("already answer", outcome.message)

    def test_question_the_agent_owns_blocks(self):
        outcome = gates.stop_gate("Dict or list?", False, ctx_with(), asker(FakeJev(asks_user=0.8, owner="agent")))
        self.assertEqual(outcome.action, "block")

    def test_waiting_after_a_question_tool_passes(self):
        ctx = ctx_with([ToolCall("toolu_q", "AskUserQuestion", "{}")])
        outcome = gates.stop_gate("Waiting for your answer.", False, ctx, asker(FakeJev(asks_user=0.0)))
        self.assertEqual(outcome.action, "allow")

    def test_prior_question_tool_does_not_exempt_a_different_final_question(self):
        ctx = ctx_with([ToolCall("toolu_q", "AskUserQuestion", "{}")])
        outcome = gates.stop_gate("Should I use Postgres?", False, ctx,
                                  asker(FakeJev(asks_user=.95, stated=.99, owner="user")))
        self.assertEqual(outcome.action, "block")

    def test_exact_user_instruction_takes_precedence_over_binding(self):
        ctx = ctx_with([replace(DECIDE, output="ts-decide verdict: BINDING")], [BINDING])
        outcome = gates.stop_gate("Done: implemented LRU as you instructed.", False, ctx,
                                  asker(FakeJev(made_choice=.99, settled_choice=.99, follows_pick=.01)))
        self.assertEqual(outcome.action, "allow")

    def test_binding_pick_not_followed_blocks_and_restates_the_pick(self):
        ctx = ctx_with([replace(DECIDE, output="ts-decide verdict: BINDING")], [BINDING])
        fake = FakeJev(follows_pick=0.4)
        outcome = gates.stop_gate("Done: implemented LRU.", False, ctx, asker(fake))
        self.assertEqual(outcome.action, "block")
        self.assertIn("`lfu`", outcome.message)
        self.assertEqual(fake.requests[0]["state"]["binding_picks"][0]["pick"], "lfu")

    def test_previous_block_does_not_authorize_a_choice(self):
        ctx = ctx_with()
        prior = {"gate": "stop", "action": "block", "turn": ctx.transcript.turn_key}
        outcome = gates.stop_gate("I chose LRU.", False, ctx_with(entries=[prior]), asker(FakeJev(made_choice=1.0)))
        self.assertEqual(outcome.action, "block")

    def test_empty_final_message_passes_without_a_call(self):
        fake = FakeJev()
        self.assertEqual(gates.stop_gate("  ", False, ctx_with(), asker(fake)).action, "allow")
        self.assertEqual(fake.requests, [])

    def test_jev_failure_allows_with_a_note(self):
        outcome = gates.stop_gate("I chose LRU.", False, ctx_with(), asker(FakeJev(raises=jev.JevError("timeout"))))
        self.assertEqual((outcome.action, "timeout" in outcome.note), ("allow", True))
