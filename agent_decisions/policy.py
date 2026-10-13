"""Every tunable value and the ownership policy, in one place. Tune from the ledger with Rex."""

from __future__ import annotations

import os
from pathlib import Path

MODEL = "jev-1.13.0"
REVIEW_CONTRACT = "scoped-review-2"
URL = "https://api.typesafe.ai/v1/systemone"
TIMEOUT_S = 3.0
KEY_FILE = Path.home() / ".config" / "sops-nix" / "secrets" / "typesafe_api_key"

USER_MESSAGES = 5
EVIDENCE_CHARS = 80_000  # 20k tokens at about 4 characters per token
CLIP_CHARS = 16_000  # 4k tokens; a longer text keeps its head and tail
FINAL_MESSAGE_CHARS = 16_000
REQUEST_CHARS = 16_000
MIN_FACT_CHARS = 12  # shorter facts match almost anything

REFUSE_AT = 0.6
BIND_AT = 0.8
STOP_AT = 0.7
FOLLOW_AT = 0.5
REFUSAL_CAP = 2
RETAIN_DAYS = 14
CACHED_REVIEWS = 16

SHELL_TOOLS = frozenset({"Bash", "exec_command", "shell", "local_shell"})
EDIT_TOOLS = frozenset({"Edit", "Write", "MultiEdit", "NotebookEdit", "apply_patch",
                        "mcp__filesystem__write_file", "mcp__filesystem__edit_file"})
READ_TOOLS = frozenset({"Read", "Grep", "Glob"})
QUESTION_TOOLS = frozenset({"AskUserQuestion", "request_user_input", "request_user_input_async"})

POLICY = (
    "The user decides product behaviour and anything destructive, irreversible, "
    "external, or paid. The agent decides engineering details and anything that "
    "the user's words or the evidence already settle. A later specific user instruction "
    "supersedes any earlier Jev verdict, including a binding pick. User instructions take precedence."
)


def state_dir() -> Path:
    override = os.environ.get("AGENT_DECISIONS_STATE")
    return Path(override) if override else Path.home() / ".local" / "state" / "agent-decisions"


def installation_policy(host: str) -> dict:
    """Return the latest verified owner policy for this host."""
    from . import ledger
    for entry in reversed(ledger.entries("installation")):
        spec = entry.get("spec")
        if entry.get("gate") == "installation" and isinstance(spec, dict) and spec.get("host") == host:
            return spec
    return {}


def review_mode(host: str) -> str:
    """Only the protected, signed installation policy can permit unchecked actions."""
    return "availability" if installation_policy(host).get("review_mode") == "availability" else "required"


def session_review_exception(host: str, session_id: str) -> bool:
    sessions = installation_policy(host).get("session_review_exceptions", [])
    return isinstance(sessions, list) and all(isinstance(item, str) for item in sessions) and session_id in sessions


def fail_open(host: str) -> bool:
    return review_mode(host) == "availability"
