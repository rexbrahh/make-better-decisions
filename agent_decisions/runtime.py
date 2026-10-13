"""Publish complete owner-installed runtime bundles outside the plugin cache."""

from __future__ import annotations

import os
import shutil
import tempfile
import uuid
from pathlib import Path


class RuntimeError(Exception):
    """The runtime cannot be published safely."""


def namespace(home: Path | None = None) -> Path:
    return (Path.home() if home is None else home).absolute() / ".local/share/agent-decisions"


def current(home: Path | None = None) -> Path:
    return namespace(home) / "current"


def _files(source: Path) -> list[Path]:
    modules = source / "agent_decisions"
    fixed = [Path("bin") / name for name in (
        "agent-decisions-install", "agent-decisions-isolate", "agent-decisions-shell-prefix", "ts-decide")]
    fixed += [Path("hooks/hook.py"), Path("skills/deciding/SKILL.md"),
              Path(".codex-plugin/plugin.json"), Path(".claude-plugin/plugin.json")]
    required_modules = ("__init__", "evidence", "gates", "hook", "install", "isolation", "jev",
                        "ledger", "policy", "provenance", "runtime", "transcript")
    paths = sorted(set(fixed + [Path("agent_decisions") / f"{name}.py" for name in required_modules]
                       + [path.relative_to(source) for path in modules.glob("*.py")]))
    for relative in paths:
        path = source / relative
        for candidate in (path, *path.parents):
            if candidate.is_symlink():
                raise RuntimeError(f"runtime source must not contain symlinks: {candidate}")
            if candidate == source:
                break
        if not path.is_file():
            raise RuntimeError(f"runtime source file is missing: {relative}")
    return paths


def publish(source: Path, home: Path | None = None) -> Path:
    """Verify a complete copy, then atomically activate it without removing old bundles."""
    source = Path(source).absolute()
    files = _files(source)
    contents = {relative: (source / relative).read_bytes() for relative in files}
    root = namespace(home)
    bundles = root / "runtimes"
    for directory in (root, bundles):
        if directory.is_symlink():
            raise RuntimeError(f"runtime directory must not be a symlink: {directory}")
        directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    stage = Path(tempfile.mkdtemp(prefix=".stage-", dir=bundles))
    pointer = root / f".current-{uuid.uuid4().hex}"
    try:
        for relative, data in contents.items():
            target = stage / relative
            target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
            target.write_bytes(data)
            target.chmod(0o700 if relative.parts[0] == "bin" else 0o600)
            if target.read_bytes() != data:
                raise RuntimeError(f"runtime copy verification failed: {relative}")
        if _files(source) != files or any((source / relative).read_bytes() != data
                                          for relative, data in contents.items()):
            raise RuntimeError("runtime source changed during publication")
        bundle = bundles / uuid.uuid4().hex
        stage.rename(bundle)
        pointer.symlink_to(bundle.relative_to(root), target_is_directory=True)
        os.replace(pointer, current(home))
    finally:
        pointer.unlink(missing_ok=True)
        if stage.exists():
            shutil.rmtree(stage)
    return current(home)
