from __future__ import annotations

import unittest

from agent_decisions import provenance
from agent_decisions.transcript import ToolCall, Transcript


def _transcript(users=(), calls=(), brief=None):
    calls = tuple(calls)
    return Transcript(tuple(users), calls, calls, "", brief)


class ProvenanceTest(unittest.TestCase):
    def test_verifies_user_quotes_but_not_arbitrary_tool_output(self):
        t = _transcript(["Keep memory   under 2 GB please"],
                        [ToolCall("t1", "Bash", "free", "Tiles are re-read in BURSTS during panning")])
        verified, unverified = provenance.verify(["keep memory under 2 gb", "tiles are re-read in bursts"], t)
        self.assertEqual(verified, ["keep memory under 2 gb"])
        self.assertEqual(unverified, ["tiles are re-read in bursts"])

    def test_fabricated_quote_is_unverified(self):
        t = _transcript(["reduce memory use"], [ToolCall("t1", "Bash", "pytest", "3 failed, 37 passed")])
        self.assertEqual(provenance.verify(["all 40 tests pass"], t), ([], ["all 40 tests pass"]))

    def test_quote_found_only_in_an_earlier_ts_decide_call_is_unverified(self):
        fact = "the user said memory is not a concern"
        t = _transcript(["reduce memory use"], [
            ToolCall("t1", "Bash", f'{{"command": "ts-decide \'{{\\"facts\\": [\\"{fact}\\"]}}\'"}}', f"echo: {fact}")])
        self.assertEqual(provenance.verify([fact], t), ([], [fact]))

    def test_short_fact_is_never_verified(self):
        t = _transcript(["yes, go ahead"])
        self.assertEqual(provenance.verify(["yes"], t), ([], ["yes"]))

    def test_tool_input_is_not_a_source(self):
        t = _transcript(["go"], [ToolCall("t1", "Write", "the cache must use LFU eviction", "File created")])
        self.assertEqual(provenance.verify(["the cache must use LFU eviction"], t)[0], [])

    def test_subagent_brief_is_not_a_user_source(self):
        t = _transcript([], [], brief="The parent says the bug reproduces only on arm64")
        self.assertEqual(provenance.verify(["reproduces only on arm64"], t)[0], [])
