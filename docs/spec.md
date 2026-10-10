# agent-decisions: Jev-assisted decisions for Claude Code and Codex

Date: 2026-10-10. Owner: Rex. Status: approved by Rex 2026-10-10.

## Purpose

Rex: "it's for decision making, not babysit the agent." The system improves
two kinds of agent decision by consulting TypeSafe's Jev model:

1. **Ask versus decide.** The agent asks the user something that the user's
   words or the evidence already settle, or decides something the user owns.
2. **Gut choice.** The agent picks an approach, design, or root-cause theory
   without comparing the options.

Rex, on cheating: "need to have more enforceable concrete measures to ensure
agents do not cheat and that the jev model on typesafe always has the most
comprehensive and accurate context and state."

Out of scope: rule policing, scope gating, destructive-command gating, and any
use inside FieldForge code or the FieldForge repository.

## Decisions (Rex, 2026-10-10)

| Topic | Decision |
| --- | --- |
| Authority, failure 1 | Binding at 0.8 or higher. |
| Authority, failure 2 | Advisory in the main agent: an override needs stated evidence. Binding in a subagent when `sufficient` is 0.8 or higher. |
| Detection, failure 2 | `ts-decide` command plus enforcing hooks. |
| Where Jev runs | In the hook, never in the agent's shell. |
| Language | Python 3 standard library. No dependencies, no build step. |
| Packaging | One repository, `~/agent-decisions`, local git, written from scratch. Claude Code plugin; Codex hook entries. |
| Coverage | Claude main agent, Claude subagents, Codex CLI, Codex desktop app. |
| Outage | Fail open, visibly. |
| Anti-cheat | Enforced by hooks, not by instructions. |

## Prior art

`Panebianco00/jev-claude` (MIT, 6 stars, reference only, no code copied).
Ideas taken: a "did the user state this?" check separate from a confident
guess; a bounded refusal count per gate so no gate can deadlock; a ledger of
every decision; the plan-approval gate; fail-open with a visible note. Ideas
rejected: a Bash risk gate (rule policing); an MCP server (it cannot tell
which session's transcript to read); agent-written facts with no verification.

## Architecture

The agent asks for a decision by running a shell command. A `PreToolUse` hook
intercepts it before it runs. The hook runs outside the agent sandbox, so it
can read the transcript, reach the API, and write state the agent cannot
write.

```
agent: ts-decide '<request json>'
  -> PreToolUse hook (unsandboxed)
       read transcript -> build evidence -> verify facts -> call Jev
       -> write ledger + verdict file
       -> updatedInput: command becomes `cat <verdict file>`
  -> agent sees the verdict as the command output
```

On Codex the hook returns the verdict as the deny reason instead of a rewrite.
A `ts-decide` that is not the whole command (inside a pipeline or compound
command) is denied with the usage text. If the hook is not loaded, the real
`bin/ts-decide` runs instead. It prints
`agent-decisions hook inactive: decision unverified` and exits 3.

### Files

| Path | Role |
| --- | --- |
| `bin/ts-decide` | Stub: runs only when no hook intercepted the call. |
| `agent_decisions/transcript.py` | Parse Claude Code and Codex transcripts into user messages and tool results. |
| `agent_decisions/evidence.py` | Build the evidence bundle: select, redact, fit to budget. |
| `agent_decisions/provenance.py` | Verify each agent fact against its cited transcript entry. |
| `agent_decisions/jev.py` | HTTP client: request, 3-second timeout, key loading, error mapping. |
| `agent_decisions/policy.py` | Every threshold and the ownership policy text, in one file. |
| `agent_decisions/ledger.py` | Append-only JSON-lines ledger and verdict files. |
| `agent_decisions/gates.py` | Gate logic: decide, question, plan, stop, ledger guard. |
| `agent_decisions/hook.py` | Hook entry point: payload in, host-specific output out, fail-open. |
| `hooks/hooks.json`, `codex/hooks.json` | Hook registrations for Claude Code and Codex. |
| `skills/deciding/SKILL.md` | When and how to call `ts-decide`. |
| `.claude-plugin/` | Claude Code plugin and local marketplace manifests. |

Each hook takes `--host claude` or `--host codex` from its install entry. The
host is never guessed from input fields.

## `ts-decide` request

The agent passes one JSON argument:

```json
{
  "decision": "Which cache eviction policy for the tile store?",
  "options": [
    {"id": "lru", "description": "Least recently used"},
    {"id": "lfu", "description": "Least frequently used"}
  ],
  "facts": [
    "tiles are re-read in bursts",
    "keep memory under 2 GB"
  ]
}
```

Each fact is a verbatim excerpt from a user message or a tool output. The hook
ignores any other field the agent adds, including any `evidence` field.

## Anti-cheat measures

### 1. Hook-owned evidence

The hook reads `transcript_path` and builds `evidence`:

- the last five user messages, verbatim;
- the most recent tool calls and results (tool name, input, output), verbatim,
  newest first. Earlier `ts-decide` calls are left out, so Jev never sees the
  agent's earlier requests or verdicts.

Secrets are redacted (API keys, bearer tokens, private keys, JWTs). Budget:
Jev accepts 32k tokens for state plus the longest question. Evidence is
capped at 20k tokens, estimated at 4 characters per token. The oldest tool
results are dropped first. User messages are never dropped. A single tool
input, tool result, or user message over 4k tokens is cut to its first and
last 2k tokens, so one large paste cannot push the request over Jev's limit.

### 2. Fact provenance

A fact is verified when, after whitespace and case normalisation, its text
appears in a user message or a tool output in the transcript. A fact shorter
than 12 characters after normalisation is never verified ("yes" matches
almost anything). Earlier
`ts-decide` calls and their verdicts are not searched, because they contain
the agent's own requests. Facts that fail go to `unverified_claims`. The Jev
question text labels these as unverified agent assertions. (Agents cannot be
assumed to see tool-use IDs, so facts cite no source.)

Residual risk: an agent can make a claim "verifiable" by printing it with a
command such as `echo`. Jev sees that command in `evidence`. Not mitigated
further in this version.

In a subagent transcript, the first "user" message is the parent agent's
brief, not the user's words. Evidence labels it `brief`.

### 3. Option completeness and argument

Two nouls ride in every decision request:

- `missing_option`: given `evidence`, is a viable option missing?
- `argued`: does the agent-written text argue for an option instead of stating
  facts?

Either one at 0.6 or above refuses the decision. The output names the reason.
At most two refusals per decision (same `decision` text) per session; the third
attempt returns a verdict marked `OVER REFUSAL CAP`.

### 4. Sufficiency before binding

`sufficient` (noul): are `evidence` and the verified facts enough to choose?
Below 0.8, a verdict is never binding, and the output tells the agent to
gather evidence with a tool call before asking again.

### 5. Follow-through

When the turn has a binding verdict, the Stop hook asks `follows_pick` (noul)
of the final message. Below 0.5, it blocks once and restates the pick.

### Ledger integrity

The ledger and verdict files sit in `~/.local/state/agent-decisions/`. A
sandboxed agent cannot write there, but agents often run single commands
unsandboxed, and in auto mode a classifier approves them, not Rex. A signing
key does not help: the hooks run as Rex's user, so an unsandboxed agent
command can read any key a hook can read. Two checks replace it:

- **Transcript cross-check.** A ledger entry counts toward "consulted this
  turn" only when its `tool_use_id` also appears in the transcript after the
  latest user message, as a shell tool call whose command contains
  `ts-decide` or the verdict file name for that ID (Claude Code may record
  the rewritten command). The harness writes the transcript, so a forged
  ledger line has no matching tool call.
- **State-directory guard.** A `PreToolUse` hook on shell, edit, and write
  tools denies any call whose input names the state directory or
  `agent-decisions` ledger files. The denial reason says why.

Residual risk: an unsandboxed command that edits both the transcript and the
ledger, or reaches the directory through an indirect path. Not mitigated in
this version.

## Jev questions

Model pinned to `jev-1.13.0`. One request per gate event. The state always
contains `policy`:

> The user decides product behaviour and anything destructive, irreversible,
> external, or paid. The agent decides engineering details and anything that
> the user's words or the evidence already settle.

| Gate | State | Questions | Rule |
| --- | --- | --- | --- |
| `ts-decide` | decision, options plus `none_fit`, verified facts, `unverified_claims`, `evidence`, `policy` | `pick` (choice), `sufficient`, `missing_option`, `argued` (nouls), `owner` (choice: user, agent) | Refuse per measure 3. P(owner=user) ≥ 0.8: ask the user. Subagent and `sufficient` ≥ 0.8: binding. Otherwise advisory. |
| Question gate (Claude `AskUserQuestion`) | the question and options, `evidence`, `policy` | `stated` (noul): do the user's messages already state the answer? `owner` (choice) | `stated` ≥ 0.8 or P(owner=agent) ≥ 0.8: deny; the agent decides and states the call. One denial per question set. |
| Plan gate (Claude `ExitPlanMode`) | none | none | Deny once when the ledger has no `ts-decide` call in this turn. |
| Stop | final message (first 4,000 characters), `evidence`, `policy` | `made_choice`, `asks_user`, `follows_pick` (when binding) (nouls) | `made_choice` ≥ 0.7 with no `ts-decide` in the turn: block once. `asks_user` ≥ 0.7 (Codex, or Claude without `AskUserQuestion` this turn): apply the question-gate rule. `follows_pick` per measure 5. |

`made_choice` excludes executing an explicit user instruction. A Stop gate
never blocks when `stop_hook_active` is true.

"This turn" means ledger entries for the session whose `tool_use_id` appears
in the transcript after the latest user message (see Ledger integrity).

## Errors

Timeout, HTTP error, 429, 529, missing key, malformed response, or unparseable
transcript: allow the action, and show a one-line `systemMessage` that names
the failure. For `ts-decide`, the output says `decision unverified` with the
reason. Never block on an outage.

Key: `TYPESAFE_API_KEY`; otherwise `~/.config/sops-nix/secrets/typesafe_api_key`.
The key is never logged or printed. `AGENT_DECISIONS_OFF=1` disables every
gate.

## Ledger

`~/.local/state/agent-decisions/sessions/<session_id>.jsonl`, one line per
gate event: time, host, gate, session ID, agent ID, tool-use ID, the full state
sent, answers, action. One file per session keeps each hook's read small
(a full state is up to about 100 KB). Each line is one `O_APPEND` write.
Session and verdict files older than 14 days are deleted by a `SessionStart`
hook. The thresholds in `policy.py` are starting values; tune them from the
ledger with Rex.

## Codex specifics

- Hook entries in `~/.codex/hooks.json` run the same scripts with
  `--host codex`; trust them with `/hooks`. Use the Codex plugin format
  instead if it is verified.
- Codex has no `AskUserQuestion` or `ExitPlanMode`: only the `ts-decide` and
  Stop gates apply.
- The skill installs to `~/.agents/skills/deciding`. A two-line pointer goes
  in `~/.codex/AGENTS.md`; the full text stays in the skill (global plus
  FieldForge AGENTS.md is about 31.6 KiB against the 32 KiB
  `project_doc_max_bytes`).

## Testing

`python3 -m unittest` with no network. Jev is replaced by an in-process fake
at the transport function. Every new test is seen failing first.

1. Transcript parsing for both hosts, from fixtures that copy the record
   shapes of real transcripts (observed 2026-10-10).
2. Evidence: budget trimming keeps user messages; redaction removes secrets.
3. Provenance: a fabricated quote, and a quote found only in an earlier
   `ts-decide` call, land in `unverified_claims`.
4. Refusals for `missing_option` and `argued`; the refusal cap.
5. Binding only with `sufficient` ≥ 0.8 in a subagent.
6. Stop: block once; `stop_hook_active`; follow-through.
7. Fail-open for timeout, 4xx, 429, 5xx, missing key.
8. The `ts-decide` command rewrite, and the stub's exit 3.

Live (`AGENT_DECISIONS_LIVE=1`): ten hand-labelled cheat cases (omitted option,
argued facts, fabricated quote) against the real API. Report the catch rate
before rollout. A no-op hook probe in the Codex desktop app.

## Rollout

1. Implement; pass the unit tests and the live cheat cases.
2. Claude Code: `claude plugin marketplace add ~/agent-decisions`; install at
   user scope.
3. Codex: hook entries, skill, AGENTS.md pointer.
4. Restart sessions from a new login terminal so `TYPESAFE_API_KEY` is set.
5. Review the ledger with Rex after one to two days.

## Not verified

- Whether the Codex desktop app runs hooks.
- Codex hook input: the shell `tool_name`, the command field and its type,
  whether `tool_use_id`, `transcript_path` and `last_assistant_message` are
  present, and whether it identifies a subagent. Codex transcripts record
  shell calls as `function_call` `exec_command` and patches as
  `custom_tool_call` `apply_patch` (observed 2026-10-10).
- Whether Codex also loads this plugin's Claude `hooks/hooks.json`: Codex
  config holds trust entries for other Claude plugins' hooks (observed).
- Whether Codex applies `updatedInput`. The design does not use it on Codex:
  the verdict is returned as a deny reason.
- Jev's catch rate on cheat cases.

## Verified by probe (2026-10-10, `docs/probes.md`)

- A Claude `PreToolUse` hook inside a subagent gets `agent_id` and the main
  session's `transcript_path`, not the subagent transcript. The hook derives
  `<session>/subagents/agent-<agent_id>.jsonl` itself.
- At `PreToolUse` time the transcript holds every earlier sequential call but
  not the current call or a parallel sibling. ts-decide runs as its own call.
- Claude applies `updatedInput.command` and keeps the original command in the
  transcript. A deny reason reaches the model.
- The API accepts `jev-1.13.0`; an 80,000-character state answers in about
  0.4 s.
