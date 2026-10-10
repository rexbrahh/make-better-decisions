"""Build the evidence Jev sees: the user's recent words and the agent's recent tool calls."""

from __future__ import annotations

import json
import re

from .policy import CLIP_CHARS, EVIDENCE_CHARS, USER_MESSAGES
from .transcript import Transcript, is_decide

REDACTED = "[REDACTED]"
_SECRETS = (
    re.compile(r"(?i)\bbearer\s+[A-Za-z0-9._~+/=-]{16,}"),
    re.compile(r"\b(?:sk|pk|rk)-[A-Za-z0-9_-]{16,}"),
    re.compile(r"\b(?:ghp|gho|ghu|ghs|ghr|github_pat)_[A-Za-z0-9_]{20,}"),
    re.compile(r"\bxox[abprs]-[A-Za-z0-9-]{10,}"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}"),
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----.*?-----END [A-Z ]*PRIVATE KEY-----", re.S),
)
_ASSIGNMENT = re.compile(
    r"(?i)\b([A-Z0-9_]*(?:API_KEY|SECRET|TOKEN|PASSWORD|PASSWD)[A-Z0-9_]*)(\s*[=:]\s*)(['\"]?)[^\s'\"]{8,}")


def redact(text: str, secrets: tuple[str, ...] = ()) -> str:
    for secret in secrets:
        if len(secret) >= 16:
            text = text.replace(secret, REDACTED)
    for pattern in _SECRETS:
        text = pattern.sub(REDACTED, text)
    return _ASSIGNMENT.sub(lambda m: f"{m[1]}{m[2]}{m[3]}{REDACTED}", text)


def clip(text: str, limit: int = CLIP_CHARS) -> str:
    if len(text) <= limit:
        return text
    half = limit // 2
    return f"{text[:half]}\n[... {len(text) - 2 * half} characters cut ...]\n{text[-half:]}"


def build(t: Transcript, secrets: tuple[str, ...] = ()) -> dict:
    def clean(text: str) -> str:
        return clip(redact(text, secrets))

    bundle: dict = {"user_messages": [clean(m) for m in t.user_messages[-USER_MESSAGES:]]}
    if t.brief is not None:
        bundle["brief"] = clean(t.brief)
    calls: list[dict] = []
    bundle["tool_calls_newest_first"] = calls
    used = len(json.dumps(bundle))
    for call in reversed(t.tool_calls):
        if is_decide(call):
            continue  # Jev never sees the agent's earlier requests or verdicts
        item = {"tool": call.name, "input": clean(call.input), "output": clean(call.output)}
        size = len(json.dumps(item)) + 2
        if used + size > EVIDENCE_CHARS:
            break
        calls.append(item)
        used += size
    return bundle
