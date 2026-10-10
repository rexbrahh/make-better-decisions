"""The gates. Each takes a Context and returns an Outcome; hook.py turns that into host output."""

from __future__ import annotations

import hashlib
import json
import os
import re
import shlex
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from . import jev, ledger, provenance
from .policy import BIND_AT, FINAL_MESSAGE_CHARS, FOLLOW_AT, POLICY, REFUSAL_CAP, REFUSE_AT, STOP_AT, state_dir
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
    user_owned = answers["owner"]["probabilities"].get("user", 0.0) >= BIND_AT
    if user_owned and reasons == ["none_fit"]:
        reasons = []  # the user chooses; an option the agent left out is theirs to name
    if reasons and not over_cap:
        action = "refused"
    elif user_owned:
        action = "ask_user"
    elif answers["sufficient"]["noul"] < BIND_AT or pick == NONE_FIT:
        action = "gather_evidence"
    elif ctx.agent_id:
        action = "binding"
    else:
        action = "advisory"
    message = _verdict(action, decision, answers, unverified, reasons, refusals, over_cap)
    return Outcome(action, message, {**record, "answers": answers, "pick": pick})


MISUSE = ("ts-decide must be the whole command, not part of a pipeline, a compound command, or a redirect.\n"
          + USAGE)
_HEREDOC = re.compile(r"\s*(?:\S*/)?ts-decide\s*<<-?\s*(['\"]?)(\w+)\1[ \t]*\n(.*?)\n[ \t]*\2\s*", re.S)
_PUNCTUATION = set("();<>|&")


def command_of(tool_input: dict) -> str:
    command = tool_input.get("command", tool_input.get("cmd", ""))
    if isinstance(command, list):
        argv = [str(a) for a in command]
        return argv[2] if len(argv) == 3 and argv[1] in ("-c", "-lc") else shlex.join(argv)
    return command if isinstance(command, str) else ""


def classify(command: str) -> tuple[str, str]:
    heredoc = _HEREDOC.fullmatch(command)
    if heredoc:
        return "decide", heredoc[3]
    try:
        lexer = shlex.shlex(command, posix=True, punctuation_chars=True)
        lexer.whitespace_split = True
        tokens = list(lexer)
    except ValueError:  # unbalanced quotes
        return ("misuse", "") if "ts-decide" in command else ("other", "")
    starts = [i for i in range(len(tokens)) if i == 0 or set(tokens[i - 1]) <= _PUNCTUATION]
    if not any(os.path.basename(tokens[i]) == "ts-decide" for i in starts):
        return "other", ""
    if len(tokens) == 2 and os.path.basename(tokens[0]) == "ts-decide":
        return "decide", tokens[1]
    return "misuse", ""


def guard(tool_input: dict) -> Outcome | None:
    text = json.dumps(tool_input, ensure_ascii=False)
    state = str(state_dir())
    markers = {state, state.replace(str(Path.home()), "~", 1), ".local/state/agent-decisions"}
    if any(marker in text for marker in markers):
        return Outcome("deny", "agent-decisions: tool calls may not read or write the decision ledger or verdict "
                               "files. ts-decide returns its verdict directly.", {"gate": "guard"})
    return None


def question_gate(tool_input: dict, ctx: Context, ask: Ask) -> Outcome:
    asked = [{"question": str(q.get("question", "")),
              "options": [str(o.get("label", "")) for o in q.get("options") or [] if isinstance(o, dict)]}
             for q in tool_input.get("questions") or [] if isinstance(q, dict)]
    if not asked:
        return Outcome("allow")
    key = hashlib.sha256(json.dumps([a["question"] for a in asked]).encode()).hexdigest()[:16]
    if any(e.get("gate") == "question" and e.get("action") == "deny" and e.get("question_key") == key
           for e in ctx.entries):
        return Outcome("allow")  # one denial per question set
    state = {"policy": POLICY, "questions": asked, "evidence": ctx.evidence}
    questions = {}
    for i in range(len(asked)):
        questions[f"stated_{i}"] = _noul(
            f"Do the user's messages in `evidence` already state the answer to `questions[{i}].question`?",
            "The user's own words already give the answer.", "The user has not answered this.")
        questions[f"owner_{i}"] = _owner_question(f"`questions[{i}].question`")
    try:
        answers = ask(state, questions)
    except jev.JevError as error:
        return Outcome("allow", note=f"agent-decisions: question gate skipped (jev {error.reason})")
    problems = []
    for i, item in enumerate(asked):
        if answers[f"stated_{i}"]["noul"] >= BIND_AT:
            problems.append(f"- {item['question']!r}: the user's messages already answer this. Use their answer "
                            "and quote it.")
        elif answers[f"owner_{i}"]["probabilities"].get("agent", 0.0) >= BIND_AT:
            problems.append(f"- {item['question']!r}: this is your call under the policy. Decide it (ts-decide "
                            "for a choice between approaches) and state the call.")
    action = "deny" if problems else "allow"
    message = "Question not sent.\n" + "\n".join(problems) if problems else ""
    return Outcome(action, message, {"gate": "question", "question_key": key, "state": state, "answers": answers})


def plan_gate(ctx: Context) -> Outcome:
    if consulted(ctx):
        return Outcome("allow")
    turn = ctx.transcript.turn_key
    if any(e.get("gate") == "plan" and e.get("turn") == turn for e in ctx.entries):
        return Outcome("allow")  # one denial per turn
    return Outcome("deny", "Plan not submitted: no ts-decide in this turn. Run ts-decide on the plan's main design "
                           "choice, then submit the plan.", {"gate": "plan"})


def stop_gate(final: str, stop_hook_active: bool, ctx: Context, ask: Ask) -> Outcome:
    if stop_hook_active:
        return Outcome("allow")
    turn = ctx.transcript.turn_key
    if any(e.get("gate") == "stop" and e.get("action") == "block" and e.get("turn") == turn for e in ctx.entries):
        return Outcome("allow")  # one block per turn
    final = final.strip()[:FINAL_MESSAGE_CHARS]
    if not final:
        return Outcome("allow")
    decided = consulted(ctx)
    binding = next((e for e in reversed(decided) if e.get("action") == "binding"), None)
    asked_with_tool = ctx.host == "claude" and any(c.name == "AskUserQuestion" for c in ctx.transcript.turn_calls)
    state = {"policy": POLICY, "final_message": final, "evidence": ctx.evidence}
    questions = {
        "made_choice": _noul("Does `final_message` report that the agent chose between two or more approaches, "
                             "designs, or root-cause theories?",
                             "The agent picked one of several approaches, designs, or theories.",
                             "No such choice, or the agent only carried out an explicit instruction from the user."),
        "asks_user": _noul("Does `final_message` ask the user to make a decision or answer a question?",
                           "It asks the user to decide or answer something.", "It asks the user nothing."),
        "stated": _noul("If `final_message` asks the user something, do the user's messages in `evidence` already "
                        "state the answer?", "The user's own words already answer it.",
                        "The user has not answered it, or nothing is asked."),
        "owner": _owner_question("the question that `final_message` asks the user, if any"),
    }
    if binding:
        state["binding_pick"] = {"decision": binding.get("decision", ""), "pick": binding.get("pick", ""),
                                 "description": binding.get("state", {}).get("options", {}).get(binding.get("pick"), "")}
        questions["follows_pick"] = _noul("Does `final_message` act on or recommend `binding_pick.pick` for "
                                          "`binding_pick.decision`?", "It follows the pick.",
                                          "It acts on or recommends something else.")
    try:
        answers = ask(state, questions)
    except jev.JevError as error:
        return Outcome("allow", note=f"agent-decisions: stop check skipped (jev {error.reason})")
    reasons = []
    if not decided and answers["made_choice"]["noul"] >= STOP_AT:
        reasons.append("You chose between approaches without ts-decide. Run ts-decide on that choice, then finish "
                       "with its verdict.")
    if not asked_with_tool and answers["asks_user"]["noul"] >= STOP_AT:
        if answers["stated"]["noul"] >= BIND_AT:
            reasons.append("Your reply asks the user something their messages already answer. Use their answer, "
                           "quote it, and continue.")
        elif answers["owner"]["probabilities"].get("agent", 0.0) >= BIND_AT:
            reasons.append("Your reply asks the user to decide something that is yours to decide. Decide it "
                           "(ts-decide for a choice between approaches), state the call, and continue.")
    if binding and answers["follows_pick"]["noul"] < FOLLOW_AT:
        reasons.append(f"The binding pick for {binding.get('decision', '')!r} is `{binding.get('pick', '')}`. "
                       "Your reply does not follow it. Follow it.")
    return Outcome("block" if reasons else "allow", "\n".join(reasons),
                   {"gate": "stop", "state": state, "answers": answers})
