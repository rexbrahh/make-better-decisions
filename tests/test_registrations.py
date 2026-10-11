from __future__ import annotations

import json
import os
import subprocess
from pathlib import Path

from tests.helpers import StateTestCase

ROOT = Path(__file__).resolve().parent.parent


def commands(path):
    hooks = json.loads((ROOT / path).read_text())["hooks"]
    return {event: [h["command"] for group in groups for h in group["hooks"]] for event, groups in hooks.items()}


class RegistrationTest(StateTestCase):
    def run_with_root(self, command, root, **env):
        command = command.replace("${CLAUDE_PLUGIN_ROOT}", str(root)).replace("${PLUGIN_ROOT}", str(root))
        return subprocess.run(["/bin/sh", "-c", command], input="{}", capture_output=True, text=True,
                              timeout=30, env={**os.environ, **env})

    def test_startup_failures_allow_visibly_without_invalid_stdout(self):
        for registration in ("hooks/hooks.json", "codex/hooks.json"):
            for event, cmds in commands(registration).items():
                for case in ("missing_file", "missing_module", "partial_output", "missing_python"):
                    with self.subTest(registration=registration, event=event, case=case):
                        root = self.dir / case
                        entry = root / "hooks/hook.py"
                        entry.parent.mkdir(parents=True, exist_ok=True)
                        if case == "missing_module":
                            entry.write_text("import nonexistent_agent_decisions_module\n")
                        elif case == "partial_output":
                            entry.write_text("print('invalid partial output'); raise RuntimeError('startup')\n")
                        env = {"PATH": str(self.dir / "no-interpreter")} if case == "missing_python" else {}
                        result = self.run_with_root(cmds[0], root, **env)
                        self.assertEqual(result.returncode, 0, result.stderr)
                        response = json.loads(result.stdout)
                        self.assertIn("action allowed", response["systemMessage"])
                        self.assertNotIn("invalid partial output", result.stdout)

    def test_healthy_denial_is_forwarded_without_allow_fallback(self):
        root = self.dir / "cache with spaces"
        entry = root / "hooks/hook.py"
        entry.parent.mkdir(parents=True)
        response = {"decision": "block", "reason": "healthy denial"}
        entry.write_text("import json; print(json.dumps(" + repr(response) + "))\n")
        for registration in ("hooks/hooks.json", "codex/hooks.json"):
            for cmds in commands(registration).values():
                result = self.run_with_root(cmds[0], root)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(json.loads(result.stdout), response)

    def run_registered(self, command):
        command = command.replace("${CLAUDE_PLUGIN_ROOT}", str(ROOT)).replace("${PLUGIN_ROOT}", str(ROOT))
        return subprocess.run(["sh", "-c", command], input="{}", capture_output=True, text=True, timeout=30,
                              env={**os.environ, "AGENT_DECISIONS_OFF": "1"})

    def test_claude_registrations_cover_every_gate_and_run(self):
        registered = commands("hooks/hooks.json")
        self.assertEqual(set(registered), {"PreToolUse", "Stop", "SubagentStop", "SessionStart", "UserPromptSubmit"})
        group = json.loads((ROOT / "hooks/hooks.json").read_text())["hooks"]["PreToolUse"][0]
        self.assertNotIn("matcher", group)  # includes mcp__filesystem__write_file and other future tools
        for event, cmds in registered.items():
            for command in cmds:
                with self.subTest(event=event):
                    self.assertIn("--host claude", command)
                    self.assertEqual(self.run_registered(command).returncode, 0)

    def test_codex_registrations_run(self):
        registered = commands("codex/hooks.json")
        self.assertEqual(set(registered), {"PreToolUse", "Stop", "SubagentStop", "SessionStart", "UserPromptSubmit"})
        for command in (c for cmds in registered.values() for c in cmds):
            with self.subTest(command=command):
                self.assertIn("--host codex", command)
                self.assertEqual(self.run_registered(command).returncode, 0)

    def test_manifests_parse(self):
        plugin = json.loads((ROOT / ".claude-plugin/plugin.json").read_text())
        market = json.loads((ROOT / ".claude-plugin/marketplace.json").read_text())
        self.assertEqual(plugin["name"], "agent-decisions")
        self.assertEqual(market["plugins"][0]["name"], "agent-decisions")
        self.assertTrue((ROOT / "skills/deciding/SKILL.md").read_text().startswith("---\nname: deciding\n"))

    def test_codex_manifest_selects_only_codex_hooks(self):
        plugin = json.loads((ROOT / ".codex-plugin/plugin.json").read_text())
        self.assertEqual(plugin["name"], "agent-decisions")
        self.assertEqual(plugin["hooks"], "./codex/hooks.json")
        self.assertEqual(plugin["version"], json.loads((ROOT / ".claude-plugin/plugin.json").read_text())["version"])
        for cmds in commands(plugin["hooks"]).values():
            for command in cmds:
                self.assertIn("${PLUGIN_ROOT}", command)
