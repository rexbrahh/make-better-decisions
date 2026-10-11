from __future__ import annotations

import hashlib
import json
import os
import time
from pathlib import Path
from unittest import mock

from agent_decisions import evidence, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import StateTestCase


class SourceObservationTest(StateTestCase):
    def test_e2e_helper_in_the_middle_keeps_head_tail_hash_and_redaction(self):
        symbol = "NumericalPolicyResolutionInputJson"
        secret = "sk-" + "q" * 40
        text = "HEAD\n" + "x" * 8_995 + secret + "x" * (13_029 - 9_000 - len(secret))
        text += f"\nvoid {symbol}() {{ serialize_inputs(); }}\n" + "y" * 32_000 + "\nTAIL"
        path = self.dir / "protocol_adapter.cc"
        path.write_text(text)

        bundle = evidence.build(Transcript(("Inspect the serialization helper.",), (), ()),
                                secrets=(secret,), query=f"Inspect {symbol} in protocol_adapter.cc.", cwd=self.dir)

        item = bundle["source_files"][0]
        self.assertIn(symbol, item["text"])
        self.assertTrue(item["text"].startswith("HEAD\n"))
        self.assertTrue(item["text"].endswith("\nTAIL"))
        self.assertNotIn(secret, item["text"])
        self.assertNotIn("q" * 20, item["text"])
        self.assertEqual(item["sha256"], hashlib.sha256(text.encode()).hexdigest())
        self.assertTrue(item["truncated"])
        self.assertLessEqual(len(item["text"]), policy.CLIP_CHARS + 100)

    def test_project_rules_precede_optional_old_paths_after_requested_helper(self):
        (self.dir / "AGENTS.md").write_text("Preserve all compatibility constraints.\n")
        (self.dir / "helper.cc").write_text("void SerializationHelper();\n")
        calls = []
        for index in range(8):
            name = f"old_{index}.cc"
            (self.dir / name).write_text("void OlderHelper();\n")
            calls.append(ToolCall(str(index), "Read", json.dumps({"file_path": name})))
        bundle = evidence.build(Transcript(("Inspect the requested helper.",), tuple(calls), ()),
                                query="Inspect helper.cc.", cwd=self.dir)
        paths = [item["path"] for item in bundle["source_files"]]
        self.assertEqual(paths[:2], ["helper.cc", "AGENTS.md"])
        self.assertEqual(len(paths), evidence._MAX_SOURCE_FILES)

    def test_e2e_helper_and_recent_check_survive_large_user_history(self):
        # The native E2E follow-up supplied this worktree path, but retained no observations.
        relative = ".worktrees/workflow-v1-review-corpus-20261010/app/editing/protocol_adapter.cc"
        helper = self.dir / relative
        helper.parent.mkdir(parents=True)
        helper.write_text("void NumericalPolicyResolutionInputJson();\n" + "// helper detail\n" * 3_000)
        original = "Preserve portable coverage and the existing adapter interface."
        latest = "Read the implementation helper before choosing the coverage repair."
        older = tuple("Constraint: preserve the adapter behavior. " + "x" * 8_000 for _ in range(100))
        check = ToolCall("helper-read", "Bash", f"read {relative}", "serialization digest check\n" + "y" * 8_000)
        t = Transcript((original, *older, latest), (check,), (check,))

        bundle = evidence.build(t, query=f"NumericalPolicyResolutionInputJson helper: {relative}.", cwd=self.dir)
        bundle["metadata"]["user_history_source"] = "signed_host_context"
        evidence._trim_to_budget(bundle)

        self.assertEqual(bundle["user_messages"][0], original)
        self.assertEqual(bundle["user_messages"][-1], latest)
        self.assertIn(relative, [item["path"] for item in bundle["source_files"]])
        self.assertIn("helper-read", [item["id"] for item in bundle["tool_calls_newest_first"]])
        self.assertGreater(bundle["metadata"]["user_messages_omitted"], 0)
        self.assertLessEqual(len(json.dumps(bundle)), policy.EVIDENCE_CHARS)

    def test_sdk_request_path_followed_by_sentence_period_is_read(self):
        request = json.loads((Path(__file__).parent / "fixtures" /
                              "sdk-save-export-requests.json").read_text())[-1]["request"]
        helper = self.dir / "libisf" / "src" / "request_open.h"
        helper.parent.mkdir(parents=True)
        helper.write_text("void RequestOpen(RequestSink& sink);\n")
        bundle = evidence.build(Transcript(("Continue SDK extraction.",), (), ()),
                                query=request["decision"], cwd=self.dir)
        self.assertIn("libisf/src/request_open.h", [item["path"] for item in bundle["source_files"]])

    def test_source_discovery_does_not_parse_large_history_after_requested_paths_fill_probe_budget(self):
        names = [f"source_{i}.cc" for i in range(16)]
        for name in names:
            (self.dir / name).write_text("void Request() {}\n")
        old = ToolCall("old-sdk-log", "Bash", "cat old.cc", "unrelated " * 200_000)
        t = Transcript(("Inspect the requested sources.",), (old,), (old,))
        started = time.monotonic()
        sources = evidence._source_files(t, " ".join(names), self.dir, ())
        elapsed = time.monotonic() - started
        self.assertEqual([item["path"] for item in sources[0]], names[:8])
        self.assertLess(elapsed, 1.0)

    def test_sdk_helper_read_survives_older_current_turn_path_noise(self):
        helper = self.dir / "libisf" / "src" / "request_open.cc"
        helper.parent.mkdir(parents=True)
        helper.write_text("void RequestOpen(RequestSink& sink) { sink.EnsureStarted(); }\n")
        calls = [ToolCall(f"old{i}", "Bash", f"cat missing_{i}.cc", "unrelated source") for i in range(30)]
        calls.append(ToolCall("sdk-helper", "Bash", "cat libisf/src/request_open.cc", helper.read_text()))
        bundle = evidence.build(Transcript(("Continue SDK extraction.",), tuple(calls), tuple(calls)),
                                query="Which API should implement S5 save/export extraction?", cwd=self.dir)
        self.assertIn("libisf/src/request_open.cc", [item["path"] for item in bundle["source_files"]])

    def test_explicit_sdk_helper_keeps_space_ahead_of_large_project_rule_files(self):
        (self.dir / "AGENTS.md").write_text("Project rules.\n" + "a" * 16_000)
        (self.dir / "CLAUDE.md").write_text("Additional project rules.\n" + "b" * 12_000)
        helper = self.dir / "request_driver.h"
        helper.write_text("class RequestSink {};\n" + "c" * 5_000)
        calls = (ToolCall("read", "Bash", "read baseline", "d" * 16_000),
                 ToolCall("check", "Bash", "run request tests", "e" * 16_000))
        t = Transcript(("Preserve SDK compatibility. " + "f" * 16_000,), calls, calls)
        bundle = evidence.build(t, query="Reuse request_driver.h for the request interface?", cwd=self.dir)
        self.assertIn("request_driver.h", [item["path"] for item in bundle["source_files"]])
        self.assertLessEqual(len(json.dumps(bundle)), policy.EVIDENCE_CHARS)

    def test_json_and_shell_quoted_paths_with_spaces_are_read(self):
        folder = self.dir / "project with spaces"
        folder.mkdir()
        source = folder / "config.py"
        source.write_text("VALUE = 1\n")
        for query in (json.dumps({"file_path": str(source)}),
                      json.dumps({"command": "cat '" + str(source) + "'"})):
            with self.subTest(query=query):
                bundle = evidence.build(Transcript(("Inspect configuration.",), (), ()), query=query, cwd=self.dir)
                self.assertEqual([item["path"] for item in bundle["source_files"]], ["project with spaces/config.py"])

    def test_reads_query_paths_as_verified_source_observations(self):
        source = self.dir / "agent_decisions"
        source.mkdir()
        path = source / "parser.py"
        text = "def parse():\n    return 'ok'\n"
        path.write_text(text)

        ev = evidence.build(Transcript(("Read agent_decisions/parser.py",), (), ()),
                            query="Check agent_decisions/parser.py", cwd=self.dir)

        self.assertEqual([item["path"] for item in ev["source_files"]], ["agent_decisions/parser.py"])
        item = ev["source_files"][0]
        self.assertEqual(item["source"], "hook_file_read")
        self.assertEqual(item["trust"], "verified_file_observation")
        self.assertEqual(item["sha256"], hashlib.sha256(text.encode()).hexdigest())
        self.assertEqual(item["text"], text)
        self.assertFalse(item["truncated"])
        self.assertLessEqual(len(json.dumps(ev)), policy.EVIDENCE_CHARS)

    def test_includes_project_agent_rules_when_present(self):
        (self.dir / "AGENTS.md").write_text("Always inspect code before acting.\n")
        (self.dir / "CLAUDE.md").write_text("Do not trust shell echoes.\n")

        ev = evidence.build(Transcript(("Decide safely.",), (), ()), cwd=self.dir)

        paths = [item["path"] for item in ev["source_files"]]
        self.assertEqual(paths, ["AGENTS.md", "CLAUDE.md"])

    def test_excludes_private_and_out_of_root_paths(self):
        (self.dir / ".env").write_text("TOKEN=secret-secret-secret\n")
        outside = self.dir.parent / f"{self.dir.name}-outside.txt"
        outside.write_text("outside\n")
        self.addCleanup(outside.unlink, missing_ok=True)
        (self.dir / "leak.txt").symlink_to(outside)

        ev = evidence.build(
            Transcript(("Check .env and leak.txt",), (), ()),
            query=".env leak.txt",
            cwd=self.dir,
        )

        self.assertEqual(ev.get("source_files", []), [])
        self.assertEqual(ev["metadata"]["source_files_omitted"], 2)

    def test_source_text_is_bounded(self):
        path = self.dir / "large.py"
        path.write_text("x" * (policy.CLIP_CHARS + 500))

        ev = evidence.build(Transcript(("Read large.py",), (), ()), query="large.py", cwd=self.dir)

        self.assertEqual(ev["source_files"][0]["path"], "large.py")
        self.assertTrue(ev["source_files"][0]["truncated"])
        self.assertLessEqual(len(ev["source_files"][0]["text"]), policy.CLIP_CHARS + 100)

    def test_too_large_source_is_omitted_without_hashing_contents(self):
        path = self.dir / "huge.py"
        with path.open("wb") as f:
            f.truncate(2_000_000)

        ev = evidence.build(Transcript(("Read huge.py",), (), ()), query="huge.py", cwd=self.dir)

        self.assertEqual(ev.get("source_files", []), [])
        self.assertEqual(ev["metadata"]["source_files_too_large"], 1)

    def test_output_path_spam_cannot_force_unbounded_source_probes(self):
        wanted = self.dir / "wanted.py"
        wanted.write_text("wanted = True\n")
        spam = " ".join(f"missing_{i}.py" for i in range(200))
        call = ToolCall("spam", "Bash", "cat wanted.py", spam)
        original = evidence._resolve_source
        probes = []

        def counted(root, raw):
            probes.append(raw)
            return original(root, raw)

        with mock.patch.object(evidence, "_resolve_source", side_effect=counted):
            ev = evidence.build(Transcript(("Inspect wanted.py",), (call,), (call,)),
                                query="wanted.py", cwd=self.dir)

        self.assertEqual([item["path"] for item in ev["source_files"]], ["wanted.py"])
        self.assertLessEqual(len(probes), 16)

    def test_policy_state_dir_is_excluded_without_banning_project_state_sources(self):
        project_state = self.dir / "state"
        project_state.mkdir()
        (project_state / "foo.py").write_text("ok = True\n")
        runtime_state = self.dir / "runtime-private"
        runtime_state.mkdir()
        (runtime_state / "secret.py").write_text("hidden = True\n")
        saved = os.environ.get("AGENT_DECISIONS_STATE")
        os.environ["AGENT_DECISIONS_STATE"] = str(runtime_state)
        self.addCleanup(lambda: os.environ.pop("AGENT_DECISIONS_STATE", None)
                        if saved is None else os.environ.__setitem__("AGENT_DECISIONS_STATE", saved))

        ev = evidence.build(
            Transcript(("Read state/foo.py and runtime-private/secret.py",), (), ()),
            query="state/foo.py runtime-private/secret.py",
            cwd=self.dir,
        )

        self.assertEqual([item["path"] for item in ev["source_files"]], ["state/foo.py"])

    def test_project_local_campaign_journal_is_allowed(self):
        journal = self.dir / ".local" / "v45-campaign" / "journal.jsonl"
        journal.parent.mkdir(parents=True)
        text = '{"event": "append-only campaign"}\n'
        journal.write_text(text)

        ev = evidence.build(
            Transcript(("Read .local/v45-campaign/journal.jsonl",), (), ()),
            query=".local/v45-campaign/journal.jsonl",
            cwd=self.dir,
        )

        self.assertEqual([item["path"] for item in ev["source_files"]],
                         [".local/v45-campaign/journal.jsonl"])
        self.assertEqual(ev["source_files"][0]["sha256"], hashlib.sha256(text.encode()).hexdigest())
        self.assertEqual(ev["source_files"][0]["text"], text)

    def test_bounded_read_preserves_actual_tail_before_clipping(self):
        tail = "ACTUAL_TAIL"
        path = self.dir / "tail.py"
        text = "HEAD" + ("m" * 80_000) + tail
        path.write_text(text)

        ev = evidence.build(Transcript(("Read tail.py",), (), ()), query="tail.py", cwd=self.dir)

        item = ev["source_files"][0]
        self.assertTrue(item["truncated"])
        self.assertTrue(item["text"].endswith(tail))
        self.assertEqual(item["sha256"], hashlib.sha256(text.encode()).hexdigest())

    def test_file_growth_after_stat_is_treated_as_too_large(self):
        path = self.dir / "growing.py"
        path.write_text("small\n")
        original_open = evidence._open_source_file

        class GrowingFile:
            def __init__(self, size):
                self.remaining = size

            def __enter__(self):
                return self

            def __exit__(self, *_):
                return False

            def fileno(self):
                raise OSError("no stable descriptor")

            def read(self, size=-1):
                if self.remaining <= 0:
                    return b""
                take = self.remaining if size < 0 else min(size, self.remaining)
                self.remaining -= take
                return b"x" * take

        def open_maybe_growing(candidate, root):
            if candidate == path:
                return GrowingFile(evidence._MAX_SOURCE_BYTES + 1)
            return original_open(candidate, root)

        with mock.patch.object(evidence, "_open_source_file", side_effect=open_maybe_growing):
            ev = evidence.build(Transcript(("Read growing.py",), (), ()), query="growing.py", cwd=self.dir)

        self.assertEqual(ev.get("source_files", []), [])
        self.assertEqual(ev["metadata"]["source_files_too_large"], 1)

    def test_source_changed_during_read_is_not_used(self):
        path = self.dir / "changing.py"
        path.write_text("first = True\n")
        original_open = evidence._open_source_file

        class ChangingFile:
            def __init__(self, wrapped):
                self.wrapped = wrapped

            def __enter__(self):
                self.wrapped.__enter__()
                return self

            def __exit__(self, *args):
                return self.wrapped.__exit__(*args)

            def fileno(self):
                return self.wrapped.fileno()

            def read(self, size=-1):
                data = self.wrapped.read(size)
                path.write_text("second = True\n")
                return data

        def open_then_change(candidate, root):
            wrapped = original_open(candidate, root)
            if candidate == path:
                return ChangingFile(wrapped)
            return wrapped

        with mock.patch.object(evidence, "_open_source_file", side_effect=open_then_change):
            ev = evidence.build(Transcript(("Read changing.py",), (), ()), query="changing.py", cwd=self.dir)

        self.assertEqual(ev.get("source_files", []), [])
        self.assertGreaterEqual(ev["metadata"]["source_files_omitted"], 1)

    def test_final_symlink_swap_is_not_read(self):
        target = self.dir / "victim.py"
        target.write_text("safe = True\n")
        outside = self.dir.parent / f"{self.dir.name}-outside-race.py"
        outside.write_text("stolen = True\n")
        self.addCleanup(outside.unlink, missing_ok=True)
        original_resolve = evidence._resolve_source
        swapped = {"done": False}

        def resolve_then_swap(root, raw):
            resolved = original_resolve(root, raw)
            if raw == "victim.py" and resolved is not None and not swapped["done"]:
                target.unlink()
                target.symlink_to(outside)
                swapped["done"] = True
            return resolved

        with mock.patch.object(evidence, "_resolve_source", side_effect=resolve_then_swap):
            ev = evidence.build(Transcript(("Read victim.py",), (), ()), query="victim.py", cwd=self.dir)

        self.assertEqual(ev.get("source_files", []), [])
        self.assertGreaterEqual(ev["metadata"]["source_files_omitted"], 1)

    def test_ancestor_symlink_swap_is_not_read_even_if_path_is_restored(self):
        real_dir = self.dir / "pkg"
        real_dir.mkdir()
        target = real_dir / "victim.py"
        target.write_text("safe = True\n")
        outside_dir = self.dir.parent / f"{self.dir.name}-outside-pkg"
        outside_dir.mkdir()
        outside = outside_dir / "victim.py"
        outside.write_text("stolen = True\n")
        self.addCleanup(outside_dir.rmdir)
        self.addCleanup(outside.unlink, missing_ok=True)
        original_resolve = evidence._resolve_source
        original_open = evidence._open_source_file
        swapped = {"done": False}

        def resolve_then_swap(root, raw):
            resolved = original_resolve(root, raw)
            if raw == "pkg/victim.py" and resolved is not None and not swapped["done"]:
                target.unlink()
                real_dir.rmdir()
                real_dir.symlink_to(outside_dir)
                swapped["done"] = True
            return resolved

        class RestoringFile:
            def __init__(self, wrapped):
                self.wrapped = wrapped

            def __enter__(self):
                self.wrapped.__enter__()
                return self

            def __exit__(self, *args):
                return self.wrapped.__exit__(*args)

            def fileno(self):
                return self.wrapped.fileno()

            def read(self, size=-1):
                data = self.wrapped.read(size)
                real_dir.unlink()
                real_dir.mkdir()
                target.write_text("safe = True\n")
                return data

        def open_then_restore(candidate, root):
            wrapped = original_open(candidate, root)
            if candidate == target:
                return RestoringFile(wrapped)
            return wrapped

        with mock.patch.object(evidence, "_resolve_source", side_effect=resolve_then_swap), \
                mock.patch.object(evidence, "_open_source_file", side_effect=open_then_restore):
            ev = evidence.build(Transcript(("Read pkg/victim.py",), (), ()), query="pkg/victim.py", cwd=self.dir)

        self.assertEqual(ev.get("source_files", []), [])
        self.assertGreaterEqual(ev["metadata"]["source_files_omitted"], 1)
