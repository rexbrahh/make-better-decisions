"""Verify user quotations. Command output and parent briefs can contain agent-authored claims."""

from __future__ import annotations

from .policy import MIN_FACT_CHARS
from .transcript import Transcript, question_reply_words


def normalize(text: str) -> str:
    return " ".join(text.split()).casefold()


def verify(facts: list[str], t: Transcript) -> tuple[list[str], list[str]]:
    corpus = "\0".join(normalize(words) for message in t.user_messages
                       for words in (question_reply_words(message) or (message,)))
    verified: list[str] = []
    unverified: list[str] = []
    for fact in facts:
        text = normalize(fact)
        (verified if len(text) >= MIN_FACT_CHARS and text in corpus else unverified).append(fact)
    return verified, unverified
