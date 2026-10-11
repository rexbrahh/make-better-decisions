"""Build the evidence Jev sees: user words, labelled tool observations, and source reads."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
from pathlib import Path

from .policy import CLIP_CHARS, EVIDENCE_CHARS, USER_MESSAGES, state_dir
from .transcript import Transcript, is_decide

REDACTED = "[REDACTED]"
_SECRETS = (
    re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}"),
    re.compile(r"\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}"),
    re.compile(r"\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}"),
    re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----(?:(?!-----BEGIN )[\s\S])*?-----END [A-Z ]*PRIVATE KEY-----"),
)
_ASSIGNMENT = re.compile(
    r"(?i)\b([A-Z0-9_]*(?:API_KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Z0-9_]*)(['\"]?\s*[=:]\s*)(['\"]?)[^\s'\"]{8,}")
_WORD = re.compile(r"[A-Za-z0-9_]{3,}")
_PATH_TOKEN = re.compile(
    r"(?:^|[\s\"'`(<{\[])"
    r"((?:\.{1,2}/|/)?[A-Za-z0-9_@+.-]+(?:/[A-Za-z0-9_@+.-]+)+"
    r"|[A-Za-z0-9_@+.-]+\.(?:c|cc|cpp|cxx|css|go|h|hpp|html|ini|js|json|jsx|md|py|rs|sh|toml|ts|tsx|txt|yaml|yml)"
    r"|\.env(?:\.[A-Za-z0-9_.-]+)?)"
    r"(?=$|[\s\"'`)>}\],:;])")
_CONSTRAINT_HINTS = (
    "must", "never", "always", "only", "preserve", "do not", "don't", "scope",
    "constraint", "required", "require", "non negotiable", "approved", "without authorization",
)
_RULE_FILES = ("AGENTS.md", "CLAUDE.md")
_PRIVATE_PARTS = frozenset({".git", ".hg", ".svn", "secrets", "secret", "keys"})
_PRIVATE_NAMES = frozenset({"id_rsa", "id_ed25519", "known_hosts"})
_PRIVATE_SUFFIXES = (".key", ".pem", ".p12", ".pfx")
_SOURCE_CHARS = CLIP_CHARS
_MAX_SOURCE_BYTES = 1_000_000
_MAX_SOURCE_FILES = 8
_MAX_SOURCE_CANDIDATE_PROBES = 16


def redact(text: str, secrets: tuple[str, ...] = ()) -> str:
    for secret in secrets:
        if len(secret) >= 16:
            text = text.replace(secret, REDACTED)
    for pattern in _SECRETS:
        text = pattern.sub(REDACTED, text)
    return _ASSIGNMENT.sub(lambda m: f"{m[1]}{m[2]}{m[3]}{REDACTED}", text)


def scrub(value, secrets: tuple[str, ...] = ()):
    """`redact` applied to every string in a JSON-shaped value."""
    if isinstance(value, str):
        return redact(value, secrets)
    if isinstance(value, list):
        return [scrub(v, secrets) for v in value]
    if isinstance(value, dict):
        return {k: scrub(v, secrets) for k, v in value.items()}
    return value


def clip(text: str, limit: int = CLIP_CHARS) -> str:
    if len(text) <= limit:
        return text
    half = limit // 2
    return f"{text[:half]}\n[... {len(text) - 2 * half} characters cut ...]\n{text[-half:]}"


def build(t: Transcript, secrets: tuple[str, ...] = (), *, query: str = "", cwd=None) -> dict:
    def clean(text: str) -> str:
        return clip(redact(text, secrets))

    usable_calls = [call for call in t.tool_calls if not is_decide(call)]
    metadata = {
        "evidence_budget_chars": EVIDENCE_CHARS,
        "text_clip_chars": CLIP_CHARS,
        "user_messages_total": len(t.user_messages),
        "user_messages_included": 0,
        "user_messages_omitted": 0,
        "tool_calls_total": len(usable_calls),
        "tool_calls_included": 0,
        "tool_calls_omitted": 0,
        "source_files_candidates": 0,
        "source_files_included": 0,
        "source_files_omitted": 0,
        "source_files_too_large": 0,
        "source_files_truncated": 0,
    }
    bundle: dict = {"user_messages": [], "tool_calls_newest_first": [], "metadata": metadata}

    selected_users = _select_user_messages(t.user_messages, query, clean)
    chosen_users: dict[int, str] = {}
    for _idx, text in selected_users:
        trial = {**chosen_users, _idx: text}
        trial_messages = [trial[idx] for idx in sorted(trial)]
        if _fits({**bundle, "user_messages": trial_messages}):
            chosen_users = trial
            bundle["user_messages"] = trial_messages
    metadata["user_messages_included"] = len(bundle["user_messages"])
    metadata["user_messages_omitted"] = max(0, len(t.user_messages) - len(bundle["user_messages"]))

    if t.brief is not None:
        brief = clean(t.brief)
        trial = {**bundle, "brief": brief, "brief_trust": "unverified_parent_agent_brief"}
        if _fits(trial):
            bundle.update({"brief": brief, "brief_trust": "unverified_parent_agent_brief"})
        else:
            metadata["brief_omitted"] = 1

    source_items, source_omitted, source_too_large = _source_files(t, query, cwd, secrets)
    metadata["source_files_candidates"] = len(source_items) + source_omitted + source_too_large
    if source_items or cwd is not None:
        bundle["source_files"] = []
    for item in source_items:
        trial_items = [*bundle.get("source_files", []), item]
        if _fits({**bundle, "source_files": trial_items}):
            bundle["source_files"] = trial_items
        else:
            source_omitted += 1
    metadata["source_files_included"] = len(bundle.get("source_files", []))
    metadata["source_files_omitted"] = source_omitted
    metadata["source_files_too_large"] = source_too_large
    metadata["source_files_truncated"] = sum(1 for item in bundle.get("source_files", []) if item.get("truncated"))

    selected_calls = _select_tool_calls(usable_calls, query, clean)
    chosen_calls: dict[int, dict] = {}
    for _idx, item in selected_calls:
        trial = {**chosen_calls, _idx: item}
        trial_calls = [trial[idx] for idx in sorted(trial, reverse=True)]
        if _fits({**bundle, "tool_calls_newest_first": trial_calls}):
            chosen_calls = trial
            bundle["tool_calls_newest_first"] = trial_calls
    metadata["tool_calls_included"] = len(bundle["tool_calls_newest_first"])
    metadata["tool_calls_omitted"] = max(0, len(usable_calls) - len(bundle["tool_calls_newest_first"]))
    _trim_to_budget(bundle)
    return bundle


def _fits(bundle: dict) -> bool:
    return len(json.dumps(bundle, sort_keys=True)) <= EVIDENCE_CHARS


def _trim_to_budget(bundle: dict) -> None:
    metadata = bundle.get("metadata", {})
    while not _fits(bundle) and bundle.get("tool_calls_newest_first"):
        bundle["tool_calls_newest_first"].pop()
        metadata["tool_calls_included"] = len(bundle["tool_calls_newest_first"])
        metadata["tool_calls_omitted"] = metadata.get("tool_calls_omitted", 0) + 1
    while not _fits(bundle) and bundle.get("source_files"):
        bundle["source_files"].pop()
        metadata["source_files_included"] = len(bundle["source_files"])
        metadata["source_files_omitted"] = metadata.get("source_files_omitted", 0) + 1
    while not _fits(bundle) and len(bundle.get("user_messages", [])) > 1:
        bundle["user_messages"].pop()
        metadata["user_messages_included"] = len(bundle["user_messages"])
        metadata["user_messages_omitted"] = metadata.get("user_messages_omitted", 0) + 1


def _terms(text: str) -> set[str]:
    return {term.lower() for term in _WORD.findall(text)}


def _looks_like_constraint(text: str) -> bool:
    lower = text.lower()
    return any(hint in lower for hint in _CONSTRAINT_HINTS)


def _text_score(text: str, query_terms: set[str]) -> int:
    terms = _terms(text)
    score = 0
    if query_terms:
        score += 100 * len(query_terms & terms)
    if _looks_like_constraint(text):
        score += 40
    return score


def _select_user_messages(messages: tuple[str, ...], query: str, clean) -> list[tuple[int, str]]:
    query_terms = _terms(query or "")
    ranked = []
    total = len(messages)
    for idx, message in enumerate(messages):
        terms = _terms(message)
        overlap = len(query_terms & terms)
        constrained = _looks_like_constraint(message)
        if idx == 0:
            group = 0
        elif overlap or constrained:
            group = 1
        elif idx >= max(0, total - USER_MESSAGES):
            group = 2
        else:
            group = 3
        ranked.append((group, -overlap, not constrained, -idx, idx, clean(message)))
    ranked.sort()
    return [(idx, text) for _group, _overlap, _constraint, _recent, idx, text in ranked]


def _select_tool_calls(calls: list, query: str, clean) -> list[tuple[int, dict]]:
    query_terms = _terms(query or "")
    ranked = []
    for idx, call in enumerate(calls):
        searchable = f"{call.name}\n{call.input}\n{call.output}"
        score = _text_score(searchable, query_terms)
        if not query_terms:
            score += idx
        item = {
            "id": call.id,
            "tool": call.name,
            "input": clean(call.input),
            "output": clean(call.output),
            "trust": "unverified_tool_observation",
        }
        ranked.append((score, idx, item))
    ranked.sort(key=lambda row: (-row[0], -row[1]))
    return [(idx, item) for _score, idx, item in ranked]


def _source_files(t: Transcript, query: str, cwd, secrets: tuple[str, ...]) -> tuple[list[dict], int, int]:
    if cwd is None:
        return [], 0, 0
    try:
        root = Path(cwd).resolve(strict=True)
    except OSError:
        return [], 0, 0

    candidates: dict[str, Path] = {}
    for name in _RULE_FILES:
        path = root / name
        if path.is_file():
            candidates[name] = path
    ranked_candidates, omitted = _ranked_path_candidates(t, query)
    for raw in ranked_candidates:
        path = _resolve_source(root, raw)
        if path is not None:
            rel = path.relative_to(root).as_posix()
            candidates.setdefault(rel, path)
        else:
            omitted += 1

    items: list[dict] = []
    too_large = 0
    for rel, path in candidates.items():
        if len(items) >= _MAX_SOURCE_FILES:
            omitted += 1
            continue
        item, reason = _read_source(root, rel, path, secrets)
        if item is None:
            if reason == "too_large":
                too_large += 1
            else:
                omitted += 1
        else:
            items.append(item)
    return items, omitted, too_large


def _ranked_path_candidates(t: Transcript, query: str) -> tuple[list[str], int]:
    turn_ids = {call.id for call in t.turn_calls}
    current = [call for call in t.tool_calls if call.id in turn_ids]
    older = [call for call in t.tool_calls if call.id not in turn_ids]
    buckets = [
        (0, [query]),
        (1, [call.input for call in current]),
        (2, t.user_messages),
        (3, [call.input for call in older]),
        (4, [call.output for call in current]),
        (5, [call.output for call in older]),
    ]
    ranked: list[tuple[int, int, str]] = []
    seen: set[str] = set()
    order = 0
    for priority, texts in buckets:
        for raw in _path_candidates(texts):
            if raw in seen:
                continue
            seen.add(raw)
            ranked.append((priority, order, raw))
            order += 1
    ranked.sort()
    selected = [raw for _priority, _order, raw in ranked[:_MAX_SOURCE_CANDIDATE_PROBES]]
    return selected, max(0, len(ranked) - len(selected))


def _path_candidates(texts) -> list[str]:
    found: list[str] = []
    seen: set[str] = set()
    for text in texts:
        try:
            structured = json.loads(text)
        except (ValueError, TypeError):
            structured = None
        for value in _walk_strings(structured):
            cleaned = _clean_path_candidate(value)
            if "\n" not in cleaned and len(cleaned) <= 1_024 and _looks_path_like(cleaned) and cleaned not in seen:
                seen.add(cleaned)
                found.append(cleaned)
        for part in _string_values(text):
            try:
                quoted_tokens = shlex.split(part)
            except ValueError:
                quoted_tokens = []
            for token in quoted_tokens:
                cleaned = _clean_path_candidate(token)
                if " " in cleaned and "\n" not in cleaned and _looks_path_like(cleaned) and cleaned not in seen:
                    seen.add(cleaned)
                    found.append(cleaned)
            for candidate in _PATH_TOKEN.findall(part):
                cleaned = _clean_path_candidate(candidate)
                if cleaned and cleaned not in seen:
                    seen.add(cleaned)
                    found.append(cleaned)
            for token in re.split(r"\s+", part):
                cleaned = _clean_path_candidate(token)
                if cleaned and cleaned not in seen and _looks_path_like(cleaned):
                    seen.add(cleaned)
                    found.append(cleaned)
    return found


def _string_values(text):
    if not isinstance(text, str):
        return
    yield text
    try:
        value = json.loads(text)
    except ValueError:
        return
    yield from _walk_strings(value)


def _walk_strings(value):
    if isinstance(value, str):
        yield value
    elif isinstance(value, list):
        for item in value:
            yield from _walk_strings(item)
    elif isinstance(value, dict):
        for item in value.values():
            yield from _walk_strings(item)


def _clean_path_candidate(raw: str) -> str:
    value = raw.strip().strip("\"'`“”‘’()[]{}<>,;")
    value = re.sub(r":\d+(?::\d+)?$", "", value)
    if "://" in value or not value or value in (".", ".."):
        return ""
    return value


def _looks_path_like(value: str) -> bool:
    name = Path(value).name
    return "/" in value or name == ".env" or name.startswith(".env.") or "." in name


def _resolve_source(root: Path, raw: str) -> Path | None:
    path = Path(raw)
    if not path.is_absolute():
        path = root / path
    try:
        resolved = path.resolve(strict=True)
    except OSError:
        return None
    if not _is_relative_to(resolved, root) or not resolved.is_file():
        return None
    rel = resolved.relative_to(root)
    if _is_private(root, rel, resolved):
        return None
    return resolved


def _is_relative_to(path: Path, root: Path) -> bool:
    try:
        path.relative_to(root)
        return True
    except ValueError:
        return False


def _is_private(root: Path, rel: Path, resolved: Path) -> bool:
    parts = rel.parts
    name = rel.name
    if name == ".env" or name.startswith(".env."):
        return True
    if name in _PRIVATE_NAMES or name.endswith(_PRIVATE_SUFFIXES):
        return True
    try:
        private_state = state_dir().resolve(strict=False)
    except OSError:
        private_state = None
    if private_state is not None and _is_relative_to(resolved, private_state):
        return True
    return any(part in _PRIVATE_PARTS for part in parts)


def _open_source_file(path: Path, root: Path):
    rel = path.relative_to(root)
    parts = rel.parts
    if not parts or any(part in ("", ".", "..") for part in parts):
        raise OSError("invalid relative source path")

    nofollow = getattr(os, "O_NOFOLLOW", 0)
    directory = getattr(os, "O_DIRECTORY", 0)
    fd = os.open(root, os.O_RDONLY | directory | nofollow)
    file_fd = None
    try:
        for part in parts[:-1]:
            next_fd = os.open(part, os.O_RDONLY | directory | nofollow, dir_fd=fd)
            _close_fd(fd)
            fd = next_fd
        file_fd = os.open(parts[-1], os.O_RDONLY | nofollow, dir_fd=fd)
        return os.fdopen(file_fd, "rb")
    except BaseException:
        if file_fd is not None:
            _close_fd(file_fd)
        raise
    finally:
        _close_fd(fd)


def _close_fd(fd: int) -> None:
    try:
        os.close(fd)
    except OSError:
        pass


def _same_source_observation(first, second) -> bool:
    return (
        first.st_dev == second.st_dev
        and first.st_ino == second.st_ino
        and first.st_size == second.st_size
        and getattr(first, "st_mtime_ns", None) == getattr(second, "st_mtime_ns", None)
    )


def _read_source(root: Path, rel: str, path: Path, secrets: tuple[str, ...]):
    try:
        initial = path.stat()
    except OSError:
        return None, "unreadable"
    if initial.st_size > _MAX_SOURCE_BYTES:
        return None, "too_large"
    try:
        with _open_source_file(path, root) as f:
            try:
                opened = os.fstat(f.fileno())
            except (AttributeError, OSError):
                opened = None
            if opened is not None:
                if not _same_source_observation(initial, opened):
                    return None, "unreadable"
                if opened.st_size > _MAX_SOURCE_BYTES:
                    return None, "too_large"
            try:
                final = path.resolve(strict=True)
            except OSError:
                return None, "unreadable"
            if not _is_relative_to(final, root):
                return None, "unreadable"
            final_rel = final.relative_to(root)
            if _is_private(root, final_rel, final):
                return None, "unreadable"
            data = f.read(_MAX_SOURCE_BYTES + 1)
            if opened is not None:
                try:
                    after = os.fstat(f.fileno())
                except OSError:
                    return None, "unreadable"
                if not _same_source_observation(opened, after):
                    return None, "unreadable"
    except OSError:
        return None, "unreadable"
    if len(data) > _MAX_SOURCE_BYTES:
        return None, "too_large"
    if b"\0" in data[:1024]:
        return None, "binary"
    text = data.decode("utf-8", errors="replace")
    truncated = len(text) > _SOURCE_CHARS
    text = clip(redact(text, secrets), _SOURCE_CHARS)
    return {
        "path": rel,
        "sha256": hashlib.sha256(data).hexdigest(),
        "text": text,
        "source": "hook_file_read",
        "trust": "verified_file_observation",
        "truncated": truncated,
    }, None
