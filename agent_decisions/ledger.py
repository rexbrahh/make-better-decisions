"""Append-only per-session ledger and per-call verdict files."""

from __future__ import annotations

import fcntl
import json
import os
import re
import time
from pathlib import Path

from .policy import RETAIN_DAYS, state_dir


def _safe(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9_-]", "_", name)[:128] or "_"


def _session_file(session_id: str) -> Path:
    return state_dir() / "sessions" / f"{_safe(session_id)}.jsonl"


def append(entry: dict) -> None:
    path = _session_file(entry["session_id"])
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    data = (json.dumps(entry, sort_keys=True) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)  # parallel subagents share one session file
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    finally:
        os.close(fd)  # closing releases the lock


def entries(session_id: str) -> list[dict]:
    try:
        lines = _session_file(session_id).read_text(encoding="utf-8", errors="replace").splitlines()
    except FileNotFoundError:
        return []
    rows = []
    for line in lines:
        try:
            row = json.loads(line)
        except ValueError:
            continue  # a line still being written
        if isinstance(row, dict):
            rows.append(row)
    return rows


def verdict_name(tool_use_id: str) -> str:
    return f"{_safe(tool_use_id)}.txt"


def write_verdict(tool_use_id: str, text: str) -> Path:
    path = state_dir() / "verdicts" / verdict_name(tool_use_id)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = path.with_suffix(".tmp")
    temporary.write_text(text, encoding="utf-8", errors="backslashreplace")
    os.replace(temporary, path)
    return path


def prune(now: float | None = None) -> None:
    cutoff = (time.time() if now is None else now) - RETAIN_DAYS * 86_400
    for sub in ("sessions", "verdicts"):
        directory = state_dir() / sub
        if not directory.is_dir():
            continue
        for path in directory.iterdir():
            try:
                if path.stat().st_mtime < cutoff:
                    path.unlink()
            except FileNotFoundError:
                pass  # another session pruned it first
