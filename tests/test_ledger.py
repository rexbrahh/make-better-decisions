from __future__ import annotations

import json
import multiprocessing
import os
import stat
import time
import tracemalloc

from agent_decisions import gates, ledger, policy
from tests.helpers import FakeJev, StateTestCase, asker, make_ctx


def _append_many(worker):
    for i in range(50):
        ledger.append({"session_id": "s", "worker": worker, "i": i, "pad": "x" * 5_000})


class LedgerTest(StateTestCase):
    def test_expired_cache_requires_a_fresh_review(self):
        ctx = make_ctx()
        tool_input = {"file_path": "parser.py", "new_string": "read_chunks()"}
        first = gates.edit_gate("Edit", tool_input, ctx, asker(FakeJev(needs_decision=0.0)))
        ledger.append({"session_id": "s1", "host": "claude", "agent_id": None, **first.record})
        cached = make_ctx(entries=ledger.entries("s1"))
        fake = FakeJev(needs_decision=0.95)
        self.assertEqual(gates.edit_gate("Edit", tool_input, cached, asker(fake)).action, "allow")
        self.assertEqual(fake.requests, [])
        expired = make_ctx(entries=ledger.entries("s1", cache_limit=0))
        self.assertEqual(gates.edit_gate("Edit", tool_input, expired, asker(fake)).action, "deny")
        self.assertEqual(len(fake.requests), 1)

    def test_bounded_cache_preserves_authority_and_recent_complete_payloads(self):
        decision = {"session_id": "s", "gate": "decide", "state": {"receipt": "complete"}}
        users = {"session_id": "s", "gate": "user_context", "messages": ["Original instruction."]}
        ledger.append(decision)
        ledger.append(users)
        for index, gate in enumerate(("edit", "question", "plan", "stop")):
            ledger.append({"session_id": "s", "gate": gate, "state": {"index": index},
                           "chunks": [{"state": {"index": index}}]})
        rows = ledger.entries("s", cache_limit=2)
        self.assertEqual(rows[:2], [decision, users])
        self.assertTrue(all("state" not in row and "chunks" not in row for row in rows[2:4]))
        self.assertEqual(rows[4:], ledger.entries("s")[4:])
        self.assertTrue(all("state" not in row for row in ledger.entries("s", cache_limit=0)[2:]))

    def test_optional_review_payloads_do_not_accumulate_in_memory(self):
        for index in range(100):
            ledger.append({"session_id": "s", "gate": "edit", "state": {"padding": "x" * 65_536},
                           "tool_use_id": str(index)})
        tracemalloc.start()
        try:
            rows = ledger.entries("s", cache_limit=2)
            peak = tracemalloc.get_traced_memory()[1]
        finally:
            tracemalloc.stop()
        self.assertEqual(len(rows), 100)
        self.assertEqual([row["tool_use_id"] for row in rows if "state" in row], ["98", "99"])
        self.assertLess(peak, 3_000_000)

    def test_round_trip_and_damaged_line_is_skipped(self):
        ledger.append({"session_id": "s1", "gate": "decide"})
        path = policy.state_dir() / "sessions" / "s1.jsonl"
        with open(path, "a") as f:
            f.write('{"broken')
        ledger.append({"session_id": "s1", "gate": "stop"})
        self.assertEqual([e["gate"] for e in ledger.entries("s1")], ["decide"])
        self.assertEqual(ledger.entries("other"), [])

    def test_parallel_appends_never_interleave(self):
        with multiprocessing.get_context("spawn").Pool(8) as pool:
            pool.map(_append_many, range(8))
        rows = ledger.entries("s")
        self.assertEqual(len(rows), 400)
        self.assertEqual({(r["worker"], r["i"]) for r in rows}, {(w, i) for w in range(8) for i in range(50)})
        key = ledger._key_file()
        self.assertEqual(key.parent, policy.state_dir())
        self.assertEqual(stat.S_IMODE(key.stat().st_mode), 0o600)
        self.assertEqual(len(key.read_bytes()), 32)

    def test_signed_entries_do_not_expose_internal_seal_fields(self):
        entry = {"session_id": "s1", "gate": "decide", "action": "advisory", "tool_use_id": "toolu_1"}
        ledger.append(entry)
        self.assertEqual(ledger.entries("s1"), [entry])
        self.assertFalse(any(key.startswith("_ledger") for key in ledger.entries("s1")[0]))

    def test_tampered_signed_decision_entry_is_ignored(self):
        ledger.append({"session_id": "s1", "gate": "decide", "action": "advisory", "pick": "lru",
                       "tool_use_id": "toolu_1", "agent_id": "agent_a"})
        path = policy.state_dir() / "sessions" / "s1.jsonl"
        row = json.loads(path.read_text(encoding="utf-8").splitlines()[0])
        target = row["entry"] if "entry" in row else row
        target["action"] = "binding"
        target["pick"] = "lfu"
        target["tool_use_id"] = "toolu_2"
        target["agent_id"] = "agent_b"
        path.write_text(json.dumps(row, sort_keys=True) + "\n", encoding="utf-8")
        self.assertEqual(ledger.entries("s1"), [])
        self.assertEqual(ledger.entries("s1", cache_limit=0), [])

    def test_unsigned_entry_is_ignored(self):
        ledger.append({"session_id": "s1", "gate": "decide", "action": "advisory"})
        path = policy.state_dir() / "sessions" / "s1.jsonl"
        with open(path, "a") as f:
            f.write(json.dumps({"session_id": "s1", "gate": "decide", "action": "binding"}) + "\n")
        self.assertEqual([e["action"] for e in ledger.entries("s1")], ["advisory"])

    def test_signed_entry_moved_to_another_session_is_ignored(self):
        ledger.append({"session_id": "s1", "gate": "decide", "action": "advisory", "tool_use_id": "toolu_1"})
        source = policy.state_dir() / "sessions" / "s1.jsonl"
        target = policy.state_dir() / "sessions" / "s2.jsonl"
        target.write_text(source.read_text(encoding="utf-8"), encoding="utf-8")
        self.assertEqual(ledger.entries("s2"), [])
        self.assertEqual(len(ledger.entries("s1")), 1)

    def test_corrupt_key_does_not_authorize_existing_entries(self):
        ledger.append({"session_id": "s1", "gate": "decide", "action": "advisory"})
        ledger._key_file().write_bytes(b"corrupt")
        self.assertEqual(ledger.entries("s1"), [])

    def test_verdict_file_stays_inside_the_state_directory(self):
        path = ledger.write_verdict("../../etc/x", "verdict text")
        self.assertEqual(path.parent, policy.state_dir() / "verdicts")
        self.assertEqual(path.read_text(), "verdict text")
        self.assertEqual(path.name, ledger.verdict_name("../../etc/x"))

    def test_prune_removes_only_old_files(self):
        ledger.append({"session_id": "old"})
        ledger.append({"session_id": "new"})
        old_verdict = ledger.write_verdict("old", "v")
        old = time.time() - (policy.RETAIN_DAYS + 1) * 86_400
        for path in (policy.state_dir() / "sessions" / "old.jsonl", old_verdict):
            os.utime(path, (old, old))
        ledger.prune()
        self.assertEqual(ledger.entries("old"), [])
        self.assertFalse(old_verdict.exists())
        self.assertEqual(len(ledger.entries("new")), 1)

    def test_prune_without_state_directory_is_a_no_op(self):
        ledger.prune()

    def test_installation_recipe_survives_session_retention(self):
        entry = {"session_id": "installation", "gate": "installation", "action": "configured",
                 "spec": {"host": "codex", "state_root": str(policy.state_dir()), "servers": {}}}
        ledger.append(entry)
        path = policy.state_dir() / "sessions/installation.jsonl"
        old = time.time() - (policy.RETAIN_DAYS + 1) * 86_400
        os.utime(path, (old, old))
        ledger.prune()
        self.assertEqual(ledger.entries("installation"), [entry])
