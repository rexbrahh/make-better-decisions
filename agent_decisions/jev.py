"""TypeSafe System One client. Every failure becomes a JevError with a short reason."""

from __future__ import annotations

import json
import os
import socket
import urllib.error
import urllib.request

from . import policy


class JevError(Exception):
    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


def load_key() -> str:
    key = os.environ.get("TYPESAFE_API_KEY", "").strip()
    if not key:
        try:
            key = policy.KEY_FILE.read_text().strip()
        except OSError:
            key = ""
    if not key:
        raise JevError("no_key")
    return key


def post(body: bytes, key: str, timeout: float) -> bytes:
    request = urllib.request.Request(
        policy.URL, data=body,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.read()
    except urllib.error.HTTPError as error:
        raise JevError(f"http_{error.code}") from None
    except (TimeoutError, socket.timeout):
        raise JevError("timeout") from None
    except urllib.error.URLError as error:
        timed_out = isinstance(error.reason, (TimeoutError, socket.timeout))
        raise JevError("timeout" if timed_out else "unreachable") from None
    except OSError:
        raise JevError("unreachable") from None


def ask(state: dict, questions: dict, *, key: str, post=post) -> dict[str, dict]:
    body = json.dumps({"model": policy.MODEL, "state": state, "questions": questions}).encode()
    try:
        answers = json.loads(post(body, key, policy.TIMEOUT_S))["answers"]
        for qid, question in questions.items():
            _check(answers[qid], question)
    except (ValueError, KeyError, TypeError):
        raise JevError("malformed") from None
    return answers


def _check(answer: dict, question: dict) -> None:
    if answer["type"] != question["type"]:
        raise ValueError("answer type")
    if question["type"] == "noul":
        if not 0.0 <= float(answer["noul"]) <= 1.0:
            raise ValueError("noul range")
    elif answer["choice"] not in question["criteria"] or not isinstance(answer["probabilities"], dict):
        raise ValueError("choice")
