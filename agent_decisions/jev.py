"""TypeSafe System One client. Every failure becomes a JevError with a short reason."""

from __future__ import annotations

import json
import math
import os
import socket
import time
import urllib.error
import urllib.request
from concurrent.futures import Future, TimeoutError as FutureTimeout
from threading import Thread

from . import evidence, policy


class JevError(Exception):
    def __init__(self, reason: str, detail: str | None = None):
        super().__init__(f"{reason}: {detail}" if detail else reason)
        self.reason = reason
        self.detail = detail


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
    result = Future()

    def send() -> None:
        try:
            result.set_result(_post(body, key, timeout))
        except Exception as error:
            result.set_exception(error)

    # urllib's socket timeout restarts on each read. Bound the complete exchange.
    Thread(target=send, daemon=True).start()
    try:
        return result.result(timeout=timeout)
    except FutureTimeout:
        raise JevError("timeout") from None


def _post(body: bytes, key: str, timeout: float) -> bytes:
    request = urllib.request.Request(
        policy.URL, data=body,
        headers={"Authorization": f"Bearer {key}", "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            return response.read()
    except urllib.error.HTTPError as error:
        detail = None
        if error.code == 400:
            try:
                validation = json.loads(error.read(4096)).get("detail")
                if isinstance(validation, dict) and validation.get("error_type") == "max_tokens_exceeded":
                    detail = "max_tokens_exceeded"
            except (ValueError, TypeError, AttributeError, OSError):
                pass  # Keep the HTTP failure; never echo an unknown response body.
        error.close()
        raise JevError(f"http_{error.code}", detail) from None
    except (TimeoutError, socket.timeout):
        raise JevError("timeout") from None
    except urllib.error.URLError as error:
        timed_out = isinstance(error.reason, (TimeoutError, socket.timeout))
        raise JevError("timeout" if timed_out else "unreachable") from None
    except OSError:
        raise JevError("unreachable") from None


def _context_limit_error(state: dict, questions: dict, attempt_count: int, request_bytes: int) -> JevError:
    bundle = state.get("evidence", {})
    sizes = {name: len(json.dumps(bundle.get(name, {}))) for name in (
        "user_messages", "tool_calls_newest_first", "source_files", "architecture_history", "brief")}
    sizes.update({name: len(json.dumps(state.get(name, {}))) for name in ("decisions", "pending_input")})
    sizes["longest_question"] = max((len(json.dumps(question)) for question in questions.values()), default=0)
    sizes["state"] = len(json.dumps(state))
    return JevError("http_400", f"max_tokens_exceeded; attempt_count={attempt_count}; "
                    f"request_bytes={request_bytes}; JSON characters: "
                    + ", ".join(f"{name}={size}" for name, size in sizes.items()))


def ask(state: dict, questions: dict, *, key: str, post=post, metrics: dict | None = None) -> dict[str, dict]:
    deadline = time.monotonic() + policy.TIMEOUT_S
    started = time.perf_counter()
    try:
        for attempt in range(2):
            serialization_started = time.perf_counter()
            body = json.dumps({"model": policy.MODEL, "state": state, "questions": questions}).encode()
            if metrics is not None:
                metrics.setdefault("initial_request_bytes", len(body))
                metrics.update({"request_bytes": len(body), "question_count": len(questions),
                                "requested_model": policy.MODEL, "attempt_count": attempt + 1})
                metrics["serialization_ms"] = metrics.get("serialization_ms", 0) + (
                    time.perf_counter() - serialization_started) * 1_000
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                raise JevError("timeout")
            try:
                raw = post(body, key, remaining)
                break
            except JevError as error:
                limited = error.reason == "http_400" and error.detail == "max_tokens_exceeded"
                if attempt or not limited:
                    if limited:
                        raise _context_limit_error(state, questions, attempt + 1, len(body)) from None
                    raise
                query = str(state.get("decision") or state.get("final_message") or
                            json.dumps(state.get("pending_input", {})))
                shorter = evidence.shorten_observations(state.get("evidence", {}), query, state.get("contracts", ()))
                if shorter is None:
                    raise _context_limit_error(state, questions, attempt + 1, len(body)) from None
                state["evidence"] = shorter
    finally:
        if metrics is not None:
            metrics["api_ms"] = (time.perf_counter() - started) * 1_000
    try:
        response = json.loads(raw)
        answers = response["answers"]
        if not isinstance(answers, dict):
            raise ValueError("answers object")
    except (ValueError, KeyError, TypeError):
        raise JevError("malformed", "response JSON or answers object") from None
    for qid, question in questions.items():
        try:
            _check(answers[qid], question)
        except (ValueError, KeyError, TypeError) as error:
            label = qid if isinstance(qid, str) and len(qid) <= 64 and all(
                char.isascii() and (char.isalnum() or char in "_-.") for char in qid) else "question"
            check = str(error) if isinstance(error, ValueError) else "missing or invalid fields"
            raise JevError("malformed", f"{label}: {check}") from None
    if metrics is not None:
        if isinstance(response.get("model"), str):
            metrics["model"] = response["model"][:100]
        usage = response.get("usage")
        if isinstance(usage, dict):
            metrics["usage"] = {key: value for key, value in usage.items()
                                if type(value) in (int, float) and math.isfinite(value) and value >= 0}
    return answers


def _check(answer: dict, question: dict) -> None:
    if answer["type"] != question["type"]:
        raise ValueError("answer type")
    if question["type"] == "noul":
        _probability(answer["noul"])
        return
    probabilities = answer["probabilities"]
    if answer["choice"] not in question["criteria"] or not isinstance(probabilities, dict) \
            or probabilities.keys() != question["criteria"].keys():
        raise ValueError("choice")
    _probability(answer["confidence"])
    for value in probabilities.values():
        _probability(value)
    # Decimal totals 0.99 and 1.01 can lie one binary rounding step past 0.01.
    if not math.isclose(math.fsum(probabilities.values()), 1.0, rel_tol=0, abs_tol=0.01 + math.ulp(1.0)) \
            or probabilities[answer["choice"]] != max(probabilities.values()):
        raise ValueError("choice distribution")


def _probability(value) -> None:
    if type(value) not in (int, float) or not math.isfinite(value) or not 0.0 <= value <= 1.0:
        raise ValueError("probability")
