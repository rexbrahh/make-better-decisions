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
    def run_registered(self, command):
        command = command.replace("${CLAUDE_PLUGIN_ROOT}", str(ROOT)).replace("${PLUGIN_ROOT}", str(ROOT))
        return subprocess.run(["sh", "-c", command], input="{}", capture_output=True, text=True, timeout=30,
                              env={**os.environ, "AGENT_DECISIONS_OFF": "1"})

    def test_claude_registrations_cover_every_gate_and_run(self):
        registered = commands("hooks/hooks.json")
        self.assertEqual(set(registered), {"PreToolUse", "Stop", "SubagentStop", "SessionStart"})
        matcher = json.loads((ROOT / "hooks/hooks.json").read_text())["hooks"]["PreToolUse"][0]["matcher"]
        for tool in ("Bash", "Edit", "Write", "MultiEdit", "NotebookEdit", "Read", "Grep", "Glob", "AskUserQuestion", "ExitPlanMode"):
            self.assertIn(tool, matcher.split("|"))
        for event, cmds in registered.items():
            for command in cmds:
                with self.subTest(event=event):
                    self.assertIn("--host claude", command)
                    self.assertEqual(self.run_registered(command).returncode, 0)

    def test_codex_registrations_run(self):
        registered = commands("codex/hooks.json")
        self.assertEqual(set(registered), {"PreToolUse", "Stop", "SubagentStop", "SessionStart"})
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
