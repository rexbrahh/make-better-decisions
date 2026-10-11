from __future__ import annotations

import hashlib
import json
import os
from unittest import mock

from agent_decisions import evidence, policy
from agent_decisions.transcript import ToolCall, Transcript
from tests.helpers import StateTestCase


class SourceObservationTest(StateTestCase):
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
