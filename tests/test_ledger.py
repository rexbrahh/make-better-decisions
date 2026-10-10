from __future__ import annotations

import multiprocessing
import os
import time

from agent_decisions import ledger, policy
from tests.helpers import StateTestCase


def _append_many(worker):
    for i in range(50):
        ledger.append({"session_id": "s", "worker": worker, "i": i, "pad": "x" * 5_000})


class LedgerTest(StateTestCase):
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
