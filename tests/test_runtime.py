from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path
from unittest import mock

from agent_decisions import install, isolation, ledger, policy, runtime
from tests.helpers import StateTestCase

ROOT = Path(__file__).resolve().parent.parent


class RuntimeTest(StateTestCase):
    def package(self):
        source = self.dir / "plugin-cache" / "1.1.8"
        for name in ("agent_decisions", "bin", "hooks", "skills/deciding", ".codex-plugin", ".claude-plugin"):
            shutil.copytree(ROOT / name, source / name, ignore=shutil.ignore_patterns("__pycache__"))
        return source

    def test_runtime_hook_and_launcher_survive_cache_removal(self):
        source = self.package()
        active = runtime.publish(source, home=self.dir)
        ledger.append(install.installation_entry("codex", policy.state_dir(),
                      {"echo": {"command": "/bin/cat", "args": []}}, [], None,
                      [str(runtime.namespace(self.dir))]))
        shutil.rmtree(source.parent)
        result = subprocess.run(["/usr/bin/python3", "-I", "-B", str(active / "hooks/hook.py"),
                                 "session_start", "--host", "codex"], input="{}", text=True,
                                capture_output=True, env={**os.environ, "HOME": str(self.dir)})
        self.assertEqual(result.returncode, 0, result.stderr)
        context = json.loads(result.stdout)["hookSpecificOutput"]["additionalContext"]
        self.assertIn("ts-decide", context)
        self.assertTrue((active / "bin/agent-decisions-isolate").is_file())
        result = subprocess.run([str(active / "bin/agent-decisions-isolate"), "--server", "echo", "--host", "codex"],
                                input="protocol", text=True, capture_output=True,
                                env={**os.environ, "HOME": str(self.dir)})
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "protocol")

    def test_changed_source_is_rejected_before_activation(self):
        source = self.package()
        write = Path.write_bytes
        def mutate_previous_file(path, data):
            result = write(path, data)
            if path.name == "hook.py":
                (source / ".codex-plugin/plugin.json").write_text("changed after copy")
            return result
        with mock.patch.object(Path, "write_bytes", mutate_previous_file):
            with self.assertRaises(runtime.RuntimeError):
                runtime.publish(source, home=self.dir)
        self.assertFalse(runtime.current(self.dir).exists())

    def test_native_sandbox_denies_pointer_replacement_and_bundle_writes(self):
        if sys.platform != "darwin":
            self.skipTest("native runtime protection proof requires macOS")
        active = runtime.publish(self.package(), home=self.dir)
        body = '''
from pathlib import Path
import sys
pointer = Path(sys.argv[1])
for operation in (lambda: pointer.unlink(), lambda: (pointer / "hooks/hook.py").write_text("tamper"),
                  lambda: pointer.resolve().joinpath("bin/ts-decide").write_text("tamper")):
    try:
        operation()
    except OSError:
        print("denied")
    else:
        print("allowed")
'''
        argv = isolation._wrap_argv([sys.executable, "-c", body, str(active)], policy.state_dir(),
                                   protect_code=True, read_only=False,
                                   protected_paths=[str(runtime.namespace(self.dir))])
        result = subprocess.run(argv, capture_output=True, text=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.splitlines(), ["denied"] * 3)

    def test_failed_activation_retains_previous_bundle(self):
        source = self.package()
        active = runtime.publish(source, home=self.dir)
        old = active.resolve()
        (source / "hooks/hook.py").write_text("raise RuntimeError('new fixture')\n")
        with mock.patch.object(runtime.os, "replace", side_effect=OSError("interrupted")):
            with self.assertRaises(OSError):
                runtime.publish(source, home=self.dir)
        self.assertEqual(active.resolve(), old)
        self.assertTrue((old / "agent_decisions/hook.py").is_file())
        runtime.publish(source, home=self.dir)
        self.assertNotEqual(active.resolve(), old)
        self.assertTrue(old.is_dir())

    def test_source_symlinks_and_missing_entrypoints_are_rejected(self):
        for case in ("file", "directory", "missing"):
            with self.subTest(case=case):
                source = self.package()
                path = source / ("agent_decisions" if case == "directory" else "hooks/hook.py")
                if path.is_dir():
                    shutil.rmtree(path)
                else:
                    path.unlink()
                if case != "missing":
                    path.symlink_to(ROOT / ("agent_decisions" if case == "directory" else "hooks/hook.py"))
                with self.assertRaises(runtime.RuntimeError):
                    runtime.publish(source, home=self.dir)
                self.assertFalse(runtime.current(self.dir).exists())
                shutil.rmtree(source.parent)

    def test_runtime_namespace_cannot_redirect_bundle_writes(self):
        source = self.package()
        outside = self.dir / "outside"
        outside.mkdir()
        namespace = runtime.namespace(self.dir)
        namespace.parent.mkdir(parents=True)
        namespace.symlink_to(outside, target_is_directory=True)
        with self.assertRaises(runtime.RuntimeError):
            runtime.publish(source, home=self.dir)
        self.assertEqual(list(outside.iterdir()), [])

    def test_plans_protect_namespace_and_keep_lexical_launch_paths(self):
        source = self.package()
        active = runtime.publish(source, home=self.dir)
        server = {"name": "local", "transport": {"type": "stdio", "command": "/bin/echo", "args": ["ok"]}}
        plan = install.build_codex_plan([server], self.dir / "state", source, home=self.dir)
        edit = next(e for e in plan["config_rpc"]["params"]["edits"] if e["keyPath"] == "mcp_servers.local")
        self.assertEqual(edit["value"]["command"], str(active / "bin/agent-decisions-isolate"))
        self.assertIn(str(runtime.namespace(self.dir)), plan["protected_paths"])
        filesystem = plan["config_rpc"]["params"]["edits"][0]["value"]["filesystem"]
        self.assertEqual(filesystem[str(runtime.namespace(self.dir))], "read")
        claude = install.build_claude_plan({}, self.dir / "state", source, home=self.dir)
        self.assertEqual(claude["merged_settings"]["env"]["CLAUDE_CODE_SHELL_PREFIX"],
                         str(active / "bin/agent-decisions-shell-prefix"))
        self.assertIn(str(runtime.namespace(self.dir)), claude["merged_settings"]["sandbox"]["filesystem"]["denyWrite"])
