"""Build the evidence Jev sees: user words, labelled tool observations, and source reads."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
import subprocess
from pathlib import Path

from .policy import CLIP_CHARS, EVIDENCE_CHARS, USER_MESSAGES, state_dir
from .transcript import ToolCall, Transcript, is_decide

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
    r"(?=$|[\s\"'`)>}\],:;?!])")
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
_CONTEXT_METADATA_CHARS = 256  # counts and hook.py's signed-history label
_MAX_SOURCE_QUERY_TERMS = 16


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


def _clip_source(text: str, query: str) -> str:
    existing = clip(text, _SOURCE_CHARS)
    if len(text) <= _SOURCE_CHARS:
        return existing
    terms = sorted(_terms(clip(query)), key=lambda term: (-len(term), term))[:_MAX_SOURCE_QUERY_TERMS]
    for term in terms:
        pattern = r"\b" + re.escape(term) + r"\b"
        match = re.search(pattern, text, re.I)
        if match is None:
            continue
        if re.search(pattern, existing, re.I):
            return existing
        edge = _SOURCE_CHARS // 4
        width = _SOURCE_CHARS - 2 * edge
        start = min(max(match.start() - width // 2, edge), len(text) - edge - width)
        spans = ((0, edge), (start, start + width), (len(text) - edge, len(text)))
        parts = []
        end = 0
        for left, right in spans:
            if left > end:
                parts.append(f"\n[... {left - end} characters cut ...]\n")
            parts.append(text[left:right])
            end = right
        return "".join(parts)
    return existing


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
    used_chars = len(json.dumps(bundle, sort_keys=True))

    selected_users = _select_user_messages(t.user_messages, query, clean)
    chosen_users: dict[int, str] = {}

    def include_user(idx: int, text: str) -> None:
        nonlocal used_chars
        trial = {**chosen_users, idx: text}
        trial_messages = [trial[idx] for idx in sorted(trial)]
        if _fits({**bundle, "user_messages": trial_messages}, reserved_chars=_CONTEXT_METADATA_CHARS):
            chosen_users[idx] = text
            bundle["user_messages"] = trial_messages
            used_chars = len(json.dumps(bundle, sort_keys=True))

    # Keep the original constraint and current instruction ahead of optional history.
    for idx, text in sorted(selected_users):
        if idx in (0, len(t.user_messages) - 1):
            include_user(idx, text)

    if t.brief is not None:
        brief = clean(t.brief)
        trial = {**bundle, "brief": brief, "brief_trust": "unverified_parent_agent_brief"}
        if _fits(trial, reserved_chars=_CONTEXT_METADATA_CHARS):
            bundle.update({"brief": brief, "brief_trust": "unverified_parent_agent_brief"})
            used_chars = len(json.dumps(bundle, sort_keys=True))
        else:
            metadata["brief_omitted"] = 1

    selected_calls = _select_tool_calls(usable_calls, query)
    chosen_calls: dict[int, dict] = {}

    def include_call(idx: int, call: ToolCall) -> None:
        nonlocal used_chars
        projected = {"id": call.id, "tool": call.name, "input": clip(call.input),
                     "output": clip(call.output), "trust": "unverified_tool_observation"}
        if call.execution is not None:
            projected["execution"] = scrub(call.execution, secrets)
        remaining = EVIDENCE_CHARS - _CONTEXT_METADATA_CHARS - used_chars
        if len(projected["input"]) + len(projected["output"]) > remaining:
            return  # even unescaped text cannot fit, so do not encode the full candidate
        separator = 2 if bundle["tool_calls_newest_first"] else 0
        if len(json.dumps(projected, sort_keys=True)) + separator > remaining:
            return
        # Redact complete retained texts before clipping, so boundary fragments cannot expose secrets.
        item = {**projected, "input": clean(call.input), "output": clean(call.output)}
        trial = {**chosen_calls, idx: item}
        trial_calls = [trial[index] for index in sorted(trial, reverse=True)]
        added_chars = len(json.dumps(item, sort_keys=True)) + separator
        if added_chars <= remaining:
            chosen_calls.update({idx: item})
            bundle["tool_calls_newest_first"] = trial_calls
            used_chars += added_chars

    source_items, source_omitted, source_too_large, scan_limited = _source_files(t, query, cwd, secrets)
    metadata["source_files_candidates"] = len(source_items) + source_omitted + source_too_large
    if source_items or cwd is not None:
        bundle["source_files"] = []

    def include_source(item: dict) -> None:
        nonlocal source_omitted, used_chars
        trial_items = [*bundle.get("source_files", []), item]
        if _fits({**bundle, "source_files": trial_items}, reserved_chars=_CONTEXT_METADATA_CHARS):
            bundle["source_files"] = trial_items
            used_chars = len(json.dumps(bundle, sort_keys=True))
        else:
            source_omitted += 1

    # Preserve cited native reads even when their cwd differs from the consultation root.
    requested = set(_path_candidates((query,)))
    names = {Path(path).name for path in requested}
    for idx, call in sorted(selected_calls, key=lambda item: item[0], reverse=True):
        if not call.output or not any(name in call.input for name in names):
            continue
        paths = set(_path_candidates((call.input,)))
        hits = {wanted for wanted in requested if any(
            wanted == path or wanted.endswith("/" + path) or path.endswith("/" + wanted)
            for path in paths)}
        if hits:
            include_call(idx, call)
        if len(chosen_calls) >= _MAX_SOURCE_FILES:
            break

    # Source discovery ranks the requested helper first. Older history must not displace it.
    for item in source_items[:1]:
        include_source(item)
    used_chars = len(json.dumps(bundle, sort_keys=True))
    recent_calls = sorted(selected_calls, key=lambda item: item[0], reverse=True)[:2]
    for idx, item in recent_calls:
        if idx not in chosen_calls:
            include_call(idx, item)
    for item in source_items[1:]:
        include_source(item)
    metadata["source_files_included"] = len(bundle.get("source_files", []))
    metadata["source_files_omitted"] = source_omitted
    metadata["source_files_too_large"] = source_too_large
    metadata["source_files_omitted_is_lower_bound"] = scan_limited
    metadata["source_files_truncated"] = sum(1 for item in bundle.get("source_files", []) if item.get("truncated"))

    for idx, text in selected_users:
        if idx not in chosen_users:
            include_user(idx, text)
    metadata["user_messages_included"] = len(bundle["user_messages"])
    metadata["user_messages_omitted"] = max(0, len(t.user_messages) - len(bundle["user_messages"]))
    used_chars = len(json.dumps(bundle, sort_keys=True))

    for _idx, item in selected_calls:
        if _idx not in chosen_calls:
            include_call(_idx, item)
    metadata["tool_calls_included"] = len(bundle["tool_calls_newest_first"])
    metadata["tool_calls_omitted"] = max(0, len(usable_calls) - len(bundle["tool_calls_newest_first"]))
    _trim_to_budget(bundle)
    return bundle


def _fits(bundle: dict, *, reserved_chars: int = 0) -> bool:
    return len(json.dumps(bundle, sort_keys=True)) + reserved_chars <= EVIDENCE_CHARS


def _trim_to_budget(bundle: dict) -> None:
    metadata = bundle.get("metadata", {})
    while not _fits(bundle) and bundle.get("architecture_history"):
        bundle["architecture_history"].pop()
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


def _select_tool_calls(calls: list[ToolCall], query: str) -> list[tuple[int, ToolCall]]:
    query_terms = _terms(query or "")
    ranked = []
    for idx, call in enumerate(calls):
        searchable = f"{call.name}\n{clip(call.input)}\n{clip(call.output)}"
        score = _text_score(searchable, query_terms)
        if not query_terms:
            score += idx
        ranked.append((score, idx, call))
    ranked.sort(key=lambda row: (-row[0], -row[1]))
    return [(idx, call) for _score, idx, call in ranked]


def _source_files(t: Transcript, query: str, cwd, secrets: tuple[str, ...]) -> tuple[list[dict], int, int, bool]:
    if cwd is None:
        return [], 0, 0, False
    try:
        root = Path(cwd).resolve(strict=True)
    except OSError:
        return [], 0, 0, False

    candidates: dict[str, Path] = {}
    requested = set(_path_candidates((query,)))
    requested_paths: set[str] = set()
    ranked_candidates, omitted, scan_limited = _ranked_path_candidates(t, query)
    for raw in ranked_candidates:
        path = _resolve_source(root, raw)
        if path is not None:
            rel = path.relative_to(root).as_posix()
            candidates.setdefault(rel, path)
            if raw in requested:
                requested_paths.add(rel)
        else:
            omitted += 1
    prioritized = {rel: path for rel, path in candidates.items() if rel in requested_paths}
    for name in _RULE_FILES:
        path = root / name
        if path.is_file():
            prioritized.setdefault(name, path)
    prioritized.update(candidates)

    items: list[dict] = []
    too_large = 0
    revision = _git_revision(root)
    for rel, path in prioritized.items():
        if len(items) >= _MAX_SOURCE_FILES:
            omitted += 1
            continue
        item, reason = _read_source(root, rel, path, secrets, query=query)
        if item is None:
            if reason == "too_large":
                too_large += 1
            else:
                omitted += 1
        else:
            item["git_baseline"] = _git_baseline(root, rel, revision, item["sha256"])
            items.append(item)
    return items, omitted, too_large, scan_limited


def _git_revision(root: Path) -> str | None:
    try:
        result = subprocess.run(["git", "-C", str(root), "rev-parse", "--verify", "HEAD"],
                                capture_output=True, timeout=1)
        value = result.stdout.decode("ascii", errors="replace").strip()
        return value if result.returncode == 0 and re.fullmatch(r"[a-f0-9]{40,64}", value) else None
    except (OSError, subprocess.TimeoutExpired):
        return None


def _git_baseline(root: Path, rel: str, revision: str | None, current_hash: str) -> dict:
    if revision is None:
        return {"status": "unavailable"}
    reference = f"{revision}:{rel}"
    try:
        size = subprocess.run(["git", "-C", str(root), "cat-file", "-s", reference],
                              capture_output=True, timeout=1)
        if size.returncode != 0:
            return {"revision": revision, "status": "not_in_baseline"}
        if int(size.stdout) > _MAX_SOURCE_BYTES:
            return {"revision": revision, "status": "too_large"}
        result = subprocess.run(["git", "-C", str(root), "cat-file", "blob", reference],
                                capture_output=True, timeout=1)
        if result.returncode != 0 or len(result.stdout) > _MAX_SOURCE_BYTES:
            return {"revision": revision, "status": "unavailable"}
        digest = hashlib.sha256(result.stdout).hexdigest()
        return {"revision": revision, "status": "observed", "sha256": digest,
                "differs_from_worktree": digest != current_hash}
    except (OSError, ValueError, subprocess.TimeoutExpired):
        return {"revision": revision, "status": "unavailable"}


def contract_observations(paths, cwd, secrets: tuple[str, ...]) -> dict:
    """Read bounded current hashes without promoting contract text to user authority."""
    root = Path(cwd).resolve()
    current = {}
    for rel in dict.fromkeys(paths):
        if len(current) == _MAX_SOURCE_FILES:
            break
        path = _resolve_source(root, rel)
        item, _reason = _read_source(root, rel, path, secrets) if path is not None else (None, None)
        current[rel] = {"sha256": item["sha256"] if item is not None else None,
                        "trust": "verified_file_observation" if item is not None else "unavailable"}
    return current


def architecture_history(records: list[dict], current: dict) -> list[dict]:
    """Historical decisions never grant authority. Unchecked contracts stay explicit."""
    result = []
    for record in reversed(records):
        statuses = {}
        for rel, expected in record.get("contracts", {}).items():
            actual = current.get(rel, {}).get("sha256")
            statuses[rel] = "not_checked" if rel not in current else "unavailable" if actual is None \
                else "unchanged" if actual == expected else "changed"
        result.append({**record, "contract_status": statuses, "trust": "signed_historical_decision",
                       "authorizes_write": False})
    return result


def _ranked_path_candidates(t: Transcript, query: str) -> tuple[list[str], int, bool]:
    turn_ids = {call.id for call in t.turn_calls}
    terms = _terms(query)
    calls = [(index, call) for index, call in enumerate(t.tool_calls) if not is_decide(call)]
    calls.sort(key=lambda item: (_text_score(clip(item[1].input), terms), item[0]), reverse=True)
    current = [call for _index, call in calls if call.id in turn_ids]
    older = [call for _index, call in calls if call.id not in turn_ids]
    buckets = [
        (0, [query]),
        (1, [call.input for call in current]),
        (2, reversed(t.user_messages)),
        (3, [call.input for call in older]),
        (4, [call.output for call in current]),
        (5, [call.output for call in older]),
    ]
    ranked: list[str] = []
    seen: set[str] = set()
    for _priority, texts in buckets:
        for raw in _path_candidates(texts):
            if raw in seen:
                continue
            if len(ranked) == _MAX_SOURCE_CANDIDATE_PROBES:
                return ranked, 1, True
            seen.add(raw)
            ranked.append(raw)
    return ranked, 0, False


def _path_candidates(texts):
    seen: set[str] = set()
    for text in texts:
        text = clip(text)
        try:
            structured = json.loads(text)
        except (ValueError, TypeError):
            structured = None
        for value in _walk_strings(structured):
            cleaned = _clean_path_candidate(value)
            if "\n" not in cleaned and len(cleaned) <= 1_024 and _looks_path_like(cleaned) and cleaned not in seen:
                seen.add(cleaned)
                yield cleaned
        for part in _string_values(text):
            try:
                quoted_tokens = shlex.split(part)
            except ValueError:
                quoted_tokens = []
            for token in quoted_tokens:
                cleaned = _clean_path_candidate(token)
                if " " in cleaned and "\n" not in cleaned and _looks_path_like(cleaned) and cleaned not in seen:
                    seen.add(cleaned)
                    yield cleaned
            for candidate in _PATH_TOKEN.findall(part):
                cleaned = _clean_path_candidate(candidate)
                if cleaned and cleaned not in seen:
                    seen.add(cleaned)
                    yield cleaned
            for token in re.split(r"\s+", part):
                cleaned = _clean_path_candidate(token)
                if cleaned and cleaned not in seen and _looks_path_like(cleaned):
                    seen.add(cleaned)
                    yield cleaned


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
    value = raw.strip().strip("\"'`“”‘’()[]{}<>,;").rstrip(".")
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


def _read_source(root: Path, rel: str, path: Path, secrets: tuple[str, ...], *, query: str = ""):
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
    text = _clip_source(redact(text, secrets), query)
    return {
        "path": rel,
        "sha256": hashlib.sha256(data).hexdigest(),
        "text": text,
        "source": "hook_file_read",
        "role": "working_tree_source",
        "trust": "verified_file_observation",
        "truncated": truncated,
    }, None
