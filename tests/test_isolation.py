from __future__ import annotations

import json
import os
import shlex
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from unittest import mock

from agent_decisions import isolation, ledger, policy
from tests.helpers import StateTestCase


ROOT = Path(__file__).resolve().parent.parent
ISOLATE = ROOT / "bin" / "agent-decisions-isolate"
SHELL_PREFIX = ROOT / "bin" / "agent-decisions-shell-prefix"


def _python_command(body: str) -> list[str]:
    return [sys.executable, "-c", body]


def _run(argv: list[str], *, env: dict[str, str] | None = None, stdin: str = "") -> subprocess.CompletedProcess:
    return subprocess.run(argv, text=True, input=stdin, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                          cwd=str(ROOT), env=env, timeout=20)


class IsolationRuntimeTest(StateTestCase):
    def _write_installation(self, spec: dict) -> None:
        ledger.append({"session_id": "installation", "gate": "installation", "action": "configured",
                       "time": "2026-10-11T00:00:00Z", "spec": spec})

    def _env(self, **extra: str) -> dict[str, str]:
        env = os.environ.copy()
        env["AGENT_DECISIONS_STATE"] = str(policy.state_dir())
        env.update(extra)
        return env

    def test_wrap_argv_uses_macos_sandbox_profile_without_broad_filesystem_denies(self):
        if sys.platform != "darwin":
            self.skipTest("macOS sandbox-exec profile shape is macOS-only")

        state = self.dir / "state"
        argv = isolation.wrap_argv(["/bin/echo", "ok"], state)

        self.assertEqual(argv[:3], ["/usr/bin/sandbox-exec", "-p", argv[2]])
        profile = argv[2]
        self.assertIn("(allow default)", profile)
        self.assertIn(f'(subpath "{state.resolve(strict=False)}")', profile)
        self.assertIn("file-read* file-write*", profile)
        self.assertIn("file-write*", profile)
        self.assertNotIn(str(ROOT), profile)
        self.assertEqual(argv[-2:], ["/bin/echo", "ok"])

    def test_linux_without_supported_sandbox_raises_instead_of_running_unconfined(self):
        with mock.patch("agent_decisions.isolation.sys.platform", "linux"), \
             mock.patch("agent_decisions.isolation.shutil.which", return_value=None):
            with self.assertRaises(isolation.UnsupportedIsolationError):
                isolation.wrap_argv(["/bin/echo", "ok"], self.dir / "state")

    def test_live_sandbox_preserves_stdio_and_public_files_but_blocks_computed_private_paths(self):
        if sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").exists():
            self.skipTest("live sandbox proof needs macOS sandbox-exec")

        private = self.dir / "state"
        public = self.dir / "public"
        private.mkdir()
        public.mkdir()
        (private / "secret.txt").write_text("private-ledger-secret", encoding="utf-8")
        public_file = public / "public.txt"
        public_file.write_text("public-ok", encoding="utf-8")
        public_out = public / "out.txt"
        code = r"""
import os, pathlib, sys
private = pathlib.Path(os.environ["AD_PART_A"]) / os.environ["AD_PART_B"]
public = pathlib.Path(os.environ["AD_PUBLIC"])
print("STDIN=" + sys.stdin.read())
print("PUBLIC_READ=" + (public / "public.txt").read_text())
(public / "out.txt").write_text("public-write-ok")
for op in ("read", "write"):
    try:
        if op == "read":
            (private / "secret.txt").read_text()
        else:
            (private / "new.txt").write_text("no")
    except OSError:
        print(f"PRIVATE_{op.upper()}=denied")
    else:
        print(f"PRIVATE_{op.upper()}=allowed")
"""
        env = self._env(AD_PART_A=str(private.parent), AD_PART_B=private.name, AD_PUBLIC=str(public))
        result = _run(isolation.wrap_argv(_python_command(code), private), env=env, stdin="ping")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("STDIN=ping", result.stdout)
        self.assertIn("PUBLIC_READ=public-ok", result.stdout)
        self.assertIn("PRIVATE_READ=denied", result.stdout)
        self.assertIn("PRIVATE_WRITE=denied", result.stdout)
        self.assertEqual(public_out.read_text(encoding="utf-8"), "public-write-ok")
        self.assertNotIn("private-ledger-secret", result.stdout + result.stderr)

    def test_read_only_mode_blocks_public_writes_for_interactive_exec(self):
        if sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").exists():
            self.skipTest("live sandbox proof needs macOS sandbox-exec")

        private = self.dir / "state"
        public = self.dir / "public"
        private.mkdir()
        public.mkdir()
        code = "import pathlib, os; pathlib.Path(os.environ['AD_PUBLIC']).joinpath('x').write_text('x')"
        result = _run(isolation.wrap_argv(_python_command(code), private, read_only=True),
                      env=self._env(AD_PUBLIC=str(public)))

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((public / "x").exists())

    def test_exec_command_mode_derives_private_root_from_signed_spec(self):
        if sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").exists():
            self.skipTest("live exec proof needs macOS sandbox-exec")

        private = policy.state_dir()
        public = self.dir / "public"
        private.mkdir(parents=True, exist_ok=True)
        public.mkdir()
        command = shlex.join(_python_command(
            "import os, pathlib; pathlib.Path(os.environ['AD_PUBLIC']).joinpath('x').write_text('x')"))
        self._write_installation({
            "host": "codex",
            "state_root": str(private),
            "servers": {},
            "claude_matchers": [],
            "previous_shell_prefix": None,
        })

        result = _run([sys.executable, str(ISOLATE), "exec", "--host", "codex", "--read-only",
                       "--command", command], env=self._env(AD_PUBLIC=str(public)))

        self.assertNotEqual(result.returncode, 0)
        self.assertFalse((public / "x").exists())

    def test_latest_installation_spec_uses_only_hmac_valid_rows(self):
        valid_spec = {"host": "codex", "state_root": str(self.dir / "state"),
                      "servers": {}, "claude_matchers": [], "previous_shell_prefix": None}
        self._write_installation(valid_spec)
        path = policy.state_dir() / "sessions" / "installation.jsonl"
        row = json.loads(path.read_text(encoding="utf-8").splitlines()[0])
        row["entry"]["spec"]["state_root"] = "/attacker/chosen"
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(json.dumps(row, sort_keys=True) + "\n")

        self.assertEqual(isolation.latest_installation_spec("codex")["state_root"], str(self.dir / "state"))

    def test_stdio_launcher_uses_signed_recipe_env_cwd_and_prints_no_extra_protocol_text(self):
        server_dir = self.dir / "server"
        server_dir.mkdir()
        marker = server_dir / "cwd-marker"
        code = (
            "import os, pathlib, sys; "
            "pathlib.Path('cwd-marker').write_text('ok'); "
            "sys.stdout.write(os.environ['VISIBLE'] + '|' + sys.stdin.read())"
        )
        self._write_installation({
            "host": "codex",
            "state_root": str(policy.state_dir()),
            "servers": {"echo": {"command": sys.executable, "args": ["-c", code],
                                  "env": {"VISIBLE": "ok", "SECRET_RECIPE": "do-not-print"},
                                  "env_vars": ["INHERITED_OK"], "cwd": str(server_dir), "source": "test"}},
            "claude_matchers": [],
            "previous_shell_prefix": None,
        })

        result = _run([sys.executable, str(ISOLATE), "--server", "echo", "--host", "codex"],
                      env=self._env(INHERITED_OK="yes"), stdin="hello")

        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout, "ok|hello")
        self.assertEqual(marker.read_text(encoding="utf-8"), "ok")
        self.assertNotIn("do-not-print", result.stdout + result.stderr)

    def test_shell_prefix_isolates_exact_match_and_passthrough_keeps_hook_state_access(self):
        if sys.platform != "darwin" or not Path("/usr/bin/sandbox-exec").exists():
            self.skipTest("live shell-prefix proof needs macOS sandbox-exec")

        private = policy.state_dir()
        private.mkdir(parents=True, exist_ok=True)
        (private / "secret.txt").write_text("private-ledger-secret", encoding="utf-8")
        prior_log = self.dir / "prior.log"
        prior = self.dir / "prior-prefix.sh"
        prior.write_text("#!/bin/sh\nprintf prior >> \"$AD_PRIOR_LOG\"\nexec /bin/zsh -lc \"$1\"\n",
                         encoding="utf-8")
        prior.chmod(0o700)

        match_code = r"""
import os, pathlib
private = pathlib.Path(os.environ["AD_PART_A"]) / os.environ["AD_PART_B"]
for op in ("read", "write"):
    try:
        if op == "read":
            (private / "secret.txt").read_text()
        else:
            (private / "new.txt").write_text("no")
    except OSError:
        print(f"PRIVATE_{op.upper()}=denied")
    else:
        print(f"PRIVATE_{op.upper()}=allowed")
"""
        match_command = shlex.join(_python_command(match_code))
        pass_code = (
            "import os, pathlib; "
            "p=pathlib.Path(os.environ['AD_PART_A'])/os.environ['AD_PART_B']; "
            "p.joinpath('secret.txt').read_text(); print('HOOK_READ_OK')"
        )
        pass_command = shlex.join(_python_command(pass_code))
        self._write_installation({
            "host": "claude",
            "state_root": str(private),
            "servers": {},
            "claude_matchers": [_python_command(match_code)],
            "previous_shell_prefix": str(prior),
        })
        env = self._env(AD_PART_A=str(private.parent), AD_PART_B=private.name, AD_PRIOR_LOG=str(prior_log))

        matched = _run([sys.executable, str(SHELL_PREFIX), match_command], env=env)
        passed = _run([sys.executable, str(SHELL_PREFIX), pass_command], env=env)

        self.assertEqual(matched.returncode, 0, matched.stderr)
        self.assertIn("PRIVATE_READ=denied", matched.stdout)
        self.assertIn("PRIVATE_WRITE=denied", matched.stdout)
        self.assertEqual(passed.returncode, 0, passed.stderr)
        self.assertIn("HOOK_READ_OK", passed.stdout)
        self.assertEqual(prior_log.read_text(encoding="utf-8"), "priorprior")
        self.assertNotIn("private-ledger-secret", matched.stdout + matched.stderr + passed.stdout + passed.stderr)
