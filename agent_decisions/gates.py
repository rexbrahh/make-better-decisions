"""The gates. Each takes a Context and returns an Outcome; hook.py turns that into host output."""

from __future__ import annotations

import json
import re
from dataclasses import dataclass
from typing import Callable

from . import jev, ledger, provenance
from .policy import BIND_AT, POLICY, REFUSAL_CAP, REFUSE_AT
from .transcript import Transcript, is_decide

Ask = Callable[[dict, dict], dict]
NONE_FIT = "none_fit"
_OPTION_ID = re.compile(r"[a-z0-9_-]{1,40}")

USAGE = """usage: ts-decide '<json>'   or   ts-decide <<'EOF' ... EOF
json: {"decision": "<question>",
       "options": [{"id": "<a-z0-9_->", "description": "<neutral one line>"}, ...2 or more],
       "facts": ["<verbatim excerpt from a user message or tool output>", ...]}
See the `deciding` skill."""


@dataclass(frozen=True)
class Context:
    host: str
    session_id: str
    agent_id: str | None
    tool_use_id: str
    transcript: Transcript
    evidence: dict
    entries: tuple[dict, ...]  # this session's ledger lines


@dataclass(frozen=True)
class Outcome:
    action: str
    message: str = ""
    record: dict | None = None  # gate-specific ledger fields; None writes no ledger line
    note: str = ""  # shown to the user as a systemMessage


class RequestError(ValueError):
    pass


def _noul(instructions: str, yes: str, no: str) -> dict:
    return {"type": "noul", "instructions": instructions, "criteria": {"true": yes, "false": no}}


def _owner_question(subject: str) -> dict:
    return {"type": "choice", "instructions": f"Under `policy`, who should decide {subject}?", "criteria": {
        "agent": "The agent: an engineering detail, or something the user's words or the evidence already settle.",
        "user": "The user: product behaviour, or anything destructive, irreversible, external, or paid, "
                "that the user's words and the evidence do not settle."}}


def consulted(ctx: Context) -> list[dict]:
    """Decide entries backed by a ts-decide call in this turn. A forged ledger line has no such call."""
    calls = {c.id: c for c in ctx.transcript.turn_calls if is_decide(c)}
    backed = []
    for entry in ctx.entries:
        call = calls.get(entry.get("tool_use_id"))
        if entry.get("gate") == "decide" and call is not None and (
                "ts-decide" in call.input or ledger.verdict_name(call.id) in call.input):
            backed.append(entry)
    return backed


def parse_request(raw: str) -> tuple[str, dict[str, str], list[str]]:
    try:
        request = json.loads(raw)
    except ValueError as error:
        raise RequestError(f"not JSON ({error})") from None
    if not isinstance(request, dict):
        raise RequestError("the request must be a JSON object")
    decision = request.get("decision")
    if not isinstance(decision, str) or not decision.strip():
        raise RequestError("`decision` must be a non-empty string")
    options = request.get("options")
    if not isinstance(options, list) or not 2 <= len(options) <= 254:
        raise RequestError("`options` must list 2 to 254 options")
    parsed: dict[str, str] = {}
    for option in options:
        if not isinstance(option, dict) or not isinstance(option.get("id"), str) \
                or not isinstance(option.get("description"), str) or not option["description"].strip():
            raise RequestError("each option needs a string `id` and a non-empty `description`")
        if not _OPTION_ID.fullmatch(option["id"]) or option["id"] == NONE_FIT or option["id"] in parsed:
            raise RequestError(f"option id {option['id']!r} must match [a-z0-9_-]{{1,40}}, be unique, "
                               f"and not be `{NONE_FIT}`")
        parsed[option["id"]] = option["description"]
    facts = request.get("facts", [])
    if not isinstance(facts, list) or not all(isinstance(f, str) for f in facts):
        raise RequestError("`facts` must be a list of strings")
    return decision.strip(), parsed, facts


def _decide_questions(options: dict[str, str]) -> dict:
    return {
        "pick": {"type": "choice",
                 "instructions": "Using `evidence`, `facts` and `policy`, which option in `options` best serves "
                                 "the user's stated goals for `decision`? `unverified_claims` are assertions by "
                                 "the agent that were not found in the transcript; do not treat them as evidence.",
                 "criteria": {**options, NONE_FIT: "None of the listed options fits the evidence."}},
        "sufficient": _noul("Are `evidence` and `facts` enough to choose between `options` for `decision`?",
                            "The evidence settles the choice.",
                            "Key facts are missing; more investigation is needed before choosing."),
        "missing_option": _noul("Given `evidence`, is a viable option for `decision` missing from `options`?",
                                "The evidence points to a viable option that is not listed, such as one the "
                                "user named or existing code that already does the job.",
                                "The listed options cover the viable choices."),
        "argued": _noul("Do the agent-written texts (`decision`, the descriptions in `options`, `facts`, "
                        "`unverified_claims`) argue for one option instead of neutrally stating facts?",
                        "They contain advocacy, loaded wording, or claims that one option is better.",
                        "They are neutral statements."),
        "owner": _owner_question("`decision`"),
    }


_HEADINGS = {"binding": "BINDING", "advisory": "ADVISORY", "gather_evidence": "GATHER EVIDENCE",
             "ask_user": "ASK USER", "refused": "REFUSED"}
_NEXT = {
    "binding": "Follow pick `{pick}`. It is binding for this subagent.",
    "advisory": "Take pick `{pick}`, or override it and state in your reply the evidence that outweighs it.",
    "gather_evidence": "The evidence is not enough to choose. Gather it with tool calls (read the code, run the "
                       "check), then call ts-decide again.",
    "ask_user": "The user owns this decision. Ask the user, with the options and pick `{pick}` as your "
                "recommendation.",
    "refused": "Refused ({reasons}), refusal {n} of {cap}. List every viable option and state facts as verbatim "
               "excerpts without argument, then call ts-decide again.",
}


def _verdict(action: str, decision: str, answers: dict, unverified: list[str], reasons: list[str],
             refusals: int, over_cap: bool) -> str:
    pick = answers["pick"]["choice"]
    ranked = ", ".join(f"{o} {p:.2f}" for o, p in sorted(answers["pick"]["probabilities"].items(),
                                                          key=lambda item: -item[1]))
    p_user = answers["owner"]["probabilities"].get("user", 0.0)
    head = f"ts-decide verdict: {_HEADINGS[action]}"
    if over_cap and reasons:
        head += f" (OVER REFUSAL CAP: {', '.join(reasons)} ignored)"
    lines = [head, f"decision: {decision}", f"pick: {pick}  ({ranked})",
             f"sufficient {answers['sufficient']['noul']:.2f}  missing_option {answers['missing_option']['noul']:.2f}"
             f"  argued {answers['argued']['noul']:.2f}  owner=user {p_user:.2f}"]
    if unverified:
        lines.append("unverified facts (not found verbatim in a user message or tool output): "
                     + "; ".join(json.dumps(f, ensure_ascii=False) for f in unverified))
    lines.append("next: " + _NEXT[action].format(pick=pick, reasons=", ".join(reasons), n=refusals + 1,
                                                 cap=REFUSAL_CAP))
    return "\n".join(lines)


def decide(raw: str, ctx: Context, ask: Ask) -> Outcome:
    try:
        decision, options, facts = parse_request(raw)
    except RequestError as error:
        return Outcome("invalid", f"ts-decide: invalid request: {error}\n{USAGE}")
    key = provenance.normalize(decision)
    refusals = sum(1 for e in ctx.entries
                   if e.get("gate") == "decide" and e.get("action") == "refused" and e.get("decision_key") == key)
    verified, unverified = provenance.verify(facts, ctx.transcript)
    state = {"policy": POLICY, "decision": decision, "options": options, "facts": verified,
             "unverified_claims": unverified, "evidence": ctx.evidence}
    record = {"gate": "decide", "decision": decision, "decision_key": key, "state": state}
    try:
        answers = ask(state, _decide_questions(options))
    except jev.JevError as error:
        return Outcome("unverified", f"ts-decide: decision unverified (jev {error.reason}). "
                                     "Decide yourself and state the evidence for your choice.", record)
    pick = answers["pick"]["choice"]
    reasons = [name for name, hit in (("missing_option", answers["missing_option"]["noul"] >= REFUSE_AT),
                                      ("argued", answers["argued"]["noul"] >= REFUSE_AT),
                                      ("none_fit", pick == NONE_FIT)) if hit]
    over_cap = refusals >= REFUSAL_CAP
    if reasons and not over_cap:
        action = "refused"
    elif answers["owner"]["probabilities"].get("user", 0.0) >= BIND_AT:
        action = "ask_user"
    elif answers["sufficient"]["noul"] < BIND_AT or pick == NONE_FIT:
        action = "gather_evidence"
    elif ctx.agent_id:
        action = "binding"
    else:
        action = "advisory"
    message = _verdict(action, decision, answers, unverified, reasons, refusals, over_cap)
    return Outcome(action, message, {**record, "answers": answers, "pick": pick})
