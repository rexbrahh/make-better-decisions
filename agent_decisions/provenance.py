"""Check that each agent fact is a verbatim excerpt of something the agent did not write."""

from __future__ import annotations

from .policy import MIN_FACT_CHARS
from .transcript import Transcript, is_decide


def normalize(text: str) -> str:
    return " ".join(text.split()).casefold()


def verify(facts: list[str], t: Transcript) -> tuple[list[str], list[str]]:
    sources = [*t.user_messages, *([t.brief] if t.brief else [])]
    sources += [c.output for c in t.tool_calls if not is_decide(c)]
    corpus = "\0".join(normalize(s) for s in sources)
    verified: list[str] = []
    unverified: list[str] = []
    for fact in facts:
        text = normalize(fact)
        (verified if len(text) >= MIN_FACT_CHARS and text in corpus else unverified).append(fact)
    return verified, unverified
