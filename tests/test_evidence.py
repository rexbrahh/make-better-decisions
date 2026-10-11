from __future__ import annotations

import json
import unittest

from agent_decisions import evidence, policy
from agent_decisions.transcript import ToolCall, Transcript


def _transcript(users=(), calls=(), brief=None):
    calls = tuple(calls)
    return Transcript(tuple(users), calls, calls, "", brief)


class EvidenceTest(unittest.TestCase):
    def test_recent_diagnostic_survives_older_broad_observations(self):
        query = "launcher startup capture stream denial JSON hook"
        old = [ToolCall(f"old{i}", "Read", query, query + "x" * 1_500) for i in range(80)]
        latest = ToolCall("latest", "Bash", "run comparison", "capture=valid; stream=invalid\n" + "y" * 5_000)
        ev = evidence.build(_transcript(["Preserve healthy denials."], [*old, latest]), query=query)
        self.assertIn("latest", [item["id"] for item in ev["tool_calls_newest_first"]])
        self.assertLessEqual(len(json.dumps(ev)), policy.EVIDENCE_CHARS)

    def test_budget_drops_oldest_tool_calls_and_keeps_user_messages(self):
        users = [f"ask {i}" for i in range(7)]
        calls = [ToolCall(f"t{i}", "Bash", f"cmd {i}", "y" * 15_000) for i in range(20)]
        ev = evidence.build(_transcript(users, calls))
        for message in users[-policy.USER_MESSAGES:]:
            self.assertIn(message, ev["user_messages"])
        self.assertIn(users[0], ev["user_messages"])
        kept = ev["tool_calls_newest_first"]
        self.assertGreater(len(kept), 0)
        self.assertLess(len(kept), 20)
        self.assertEqual(kept[0]["input"], "cmd 19")
        self.assertEqual(kept[0]["id"], "t19")
        self.assertEqual(kept[0]["trust"], "unverified_tool_observation")
        self.assertLessEqual(len(json.dumps(ev)), policy.EVIDENCE_CHARS)

    def test_leaves_out_ts_decide_calls(self):
        calls = [ToolCall("t1", "Bash", '{"command": "ts-decide \'{}\'"}', "verdict"),
                 ToolCall("t2", "Bash", '{"command": "ls"}', "a.txt")]
        ev = evidence.build(_transcript(["go"], calls))
        self.assertEqual([c["output"] for c in ev["tool_calls_newest_first"]], ["a.txt"])

    def test_clips_long_texts_including_user_messages(self):
        long = "head " + "m" * 50_000 + " tail"
        ev = evidence.build(_transcript([long], [ToolCall("t", "Read", long, long)]))
        for text in (ev["user_messages"][0], ev["tool_calls_newest_first"][0]["output"]):
            self.assertLessEqual(len(text), policy.CLIP_CHARS + 100)
            self.assertTrue(text.startswith("head "))
            self.assertTrue(text.endswith(" tail"))

    def test_brief_is_labelled(self):
        ev = evidence.build(_transcript([], [], brief="find the leak"))
        self.assertEqual(ev["brief"], "find the leak")
        self.assertEqual(ev["brief_trust"], "unverified_parent_agent_brief")

    def test_original_constraint_survives_newer_chatter(self):
        constraint = "Preserve atomic writes in every implementation."
        users = (constraint, *(f"Progress update {i}" for i in range(40)))
        ev = evidence.build(_transcript(users))
        self.assertIn(constraint, ev["user_messages"])
        self.assertEqual(ev["metadata"]["user_messages_total"], 41)
        self.assertEqual(ev["metadata"]["user_messages_omitted"], 0)

    def test_more_than_twenty_short_constraints_are_preserved_when_budget_fits(self):
        users = tuple(f"Constraint {i}: must preserve behaviour {i}." for i in range(35))
        ev = evidence.build(_transcript(users))
        self.assertEqual(ev["user_messages"], list(users))
        self.assertEqual(ev["metadata"]["user_messages_omitted"], 0)

    def test_query_relevance_beats_newer_irrelevant_tool_spam(self):
        relevant = ToolCall("old", "Read", "agent_decisions/evidence.py", "atomic writes are preserved")
        spam = [ToolCall(f"spam{i}", "Bash", f"cmd {i}", "noise" * 8_000) for i in range(20)]
        ev = evidence.build(_transcript(["Investigate atomic writes."], [relevant, *spam]), query="atomic writes")
        ids = [item["id"] for item in ev["tool_calls_newest_first"]]
        self.assertIn("old", ids)
        self.assertLessEqual(len(json.dumps(ev)), policy.EVIDENCE_CHARS)

    def test_redacts_secrets(self):
        samples = [
            "Authorization: Bearer " + "A" * 30,
            "key sk-" + "b" * 30,
            "token ghp_" + "c" * 36,
            "slack xoxb-" + "1234567890-abcdef",
            "aws AKIA" + "ABCDEFGHIJKLMNOP",
            "jwt eyJ" + "a" * 20 + ".eyJ" + "b" * 20 + "." + "c" * 20,
            "-----BEGIN RSA PRIVATE KEY-----\nMIIE\n-----END RSA PRIVATE KEY-----",
        ]
        for sample in samples:
            with self.subTest(sample=sample[:12]):
                self.assertIn(evidence.REDACTED, evidence.redact(sample))
        self.assertEqual(evidence.redact("TYPESAFE_API_KEY=abcdefgh12345"), f"TYPESAFE_API_KEY={evidence.REDACTED}")
        self.assertEqual(evidence.redact("plain text stays"), "plain text stays")

    def test_redacts_known_secret_literals_everywhere(self):
        key = "zz-literal-secret-0123456789"
        ev = evidence.build(_transcript([f"my key is {key}"], [ToolCall("t", "Bash", key, key)]), secrets=(key,))
        self.assertNotIn(key, json.dumps(ev))
