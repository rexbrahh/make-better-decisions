"""Append-only per-session ledger and per-call verdict files."""

from __future__ import annotations

import fcntl
import hashlib
import hmac
import json
import os
import re
import time
from pathlib import Path

from .policy import RETAIN_DAYS, state_dir


def _safe(name: str) -> str:
    return re.sub(r"[^A-Za-z0-9_-]", "_", name)[:128] or "_"


_ENTRY_FIELD = "entry"
_SEAL_FIELD = "_ledger_seal"
_SEAL_VERSION = "hmac-sha256-v1"
_KEY_BYTES = 32


def _key_file() -> Path:
    return state_dir() / "ledger.key"


def _key_lock_file() -> Path:
    return state_dir() / "ledger.key.lock"


def _session_file(session_id: str) -> Path:
    return state_dir() / "sessions" / f"{_safe(session_id)}.jsonl"


def _read_exact_key(path: Path) -> bytes | None:
    try:
        key = path.read_bytes()
    except OSError:
        return None
    return key if len(key) == _KEY_BYTES else None


def _write_new_key(path: Path) -> bytes:
    key = os.urandom(_KEY_BYTES)
    temporary = path.with_name(f".{path.name}.{os.getpid()}.{time.time_ns()}.tmp")
    try:
        fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        try:
            view = memoryview(key)
            while view:
                view = view[os.write(fd, view):]
            os.fsync(fd)
        finally:
            os.close(fd)
        os.replace(temporary, path)
        os.chmod(path, 0o600)
    except Exception:
        try:
            temporary.unlink()
        except FileNotFoundError:
            pass
        raise
    return key


def _key(create: bool) -> bytes | None:
    path = _key_file()
    if not create:
        return _read_exact_key(path)

    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    lock_fd = os.open(_key_lock_file(), os.O_RDWR | os.O_CREAT, 0o600)
    try:
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
        key = _read_exact_key(path)
        if key is not None:
            os.chmod(path, 0o600)
            return key
        if path.exists():
            raise OSError("corrupt ledger key")
        return _write_new_key(path)
    finally:
        os.close(lock_fd)


def _canonical(entry: dict) -> bytes:
    return json.dumps(entry, ensure_ascii=True, separators=(",", ":"), sort_keys=True).encode()


def _seal(entry: dict, key: bytes) -> str:
    digest = hmac.new(key, _canonical(entry), hashlib.sha256).hexdigest()
    return f"{_SEAL_VERSION}:{digest}"


def _signed_row(entry: dict, key: bytes) -> dict:
    return {_ENTRY_FIELD: entry, _SEAL_FIELD: _seal(entry, key)}


def _verified_entry(row: dict, session_id: str, key: bytes) -> dict | None:
    entry = row.get(_ENTRY_FIELD)
    seal = row.get(_SEAL_FIELD)
    if not isinstance(entry, dict) or not isinstance(seal, str):
        return None
    if entry.get("session_id") != session_id:
        return None
    if not hmac.compare_digest(seal, _seal(entry, key)):
        return None
    return entry


def append(entry: dict) -> None:
    key = _key(create=True)
    if key is None:
        raise OSError("missing ledger key")
    path = _session_file(entry["session_id"])
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    data = (json.dumps(_signed_row(entry, key), ensure_ascii=True, sort_keys=True) + "\n").encode()
    fd = os.open(path, os.O_WRONLY | os.O_APPEND | os.O_CREAT, 0o600)
    try:
        fcntl.flock(fd, fcntl.LOCK_EX)  # parallel subagents share one session file
        view = memoryview(data)
        while view:
            view = view[os.write(fd, view):]
    finally:
        os.close(fd)  # closing releases the lock


def entries(session_id: str) -> list[dict]:
    key = _key(create=False)
    if key is None:
        return []
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
            entry = _verified_entry(row, session_id, key)
            if entry is not None:
                rows.append(entry)
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


def _architecture_session(repo_id: str) -> str:
    return "architecture-" + hashlib.sha256(repo_id.encode()).hexdigest()


def architecture_path(repo_id: str) -> Path:
    return _session_file(_architecture_session(repo_id))


def architecture_entries(repo_id: str) -> list[dict]:
    rows = entries(_architecture_session(repo_id))
    return [row for row in rows if row.get("gate") == "architecture" and row.get("repo_id") == repo_id][-16:]


def remember_architecture(repo_id: str, decisions: list[dict]) -> None:
    """Called only for completed receipts verified by the host-context loader."""
    if not any(entry.get("contracts") for entry in decisions):
        return
    known = {row.get("source_receipt") for row in entries(_architecture_session(repo_id))}
    for entry in decisions:
        if entry.get("action") not in {"advisory", "binding"} or not entry.get("contracts") \
                or not entry.get("receipt_id") or entry.get("repo_id") != repo_id or entry["receipt_id"] in known:
            continue
        state = entry.get("state", {})
        scope = state.get("scope", {})
        append({"session_id": _architecture_session(repo_id), "gate": "architecture", "action": "observed",
                "repo_id": repo_id, "source_receipt": entry["receipt_id"], "time": entry.get("time"),
                "decision": entry.get("decision", "")[:500], "pick": entry["pick"],
                "selected_approach": state.get("options", {}).get(entry["pick"], "")[:1_000],
                "scope": {key: [value[:200] for value in scope.get(key, ())[:8]] for key in ("paths", "invariants")},
                "contracts": entry["contracts"]})
        known.add(entry["receipt_id"])


def prune(now: float | None = None) -> None:
    cutoff = (time.time() if now is None else now) - RETAIN_DAYS * 86_400
    for sub in ("sessions", "verdicts"):
        directory = state_dir() / sub
        if not directory.is_dir():
            continue
        for path in directory.iterdir():
            if sub == "sessions" and (path.name == "installation.jsonl" or path.name.startswith("architecture-")):
                continue  # installation policy and architectural context outlive chat retention
            try:
                if path.stat().st_mtime < cutoff:
                    path.unlink()
            except FileNotFoundError:
                pass  # another session pruned it first
