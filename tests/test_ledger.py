from __future__ import annotations

import json
import multiprocessing
import os
import stat
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
