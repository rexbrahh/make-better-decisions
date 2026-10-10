# agent-decisions: Jev-assisted decisions for Claude Code and Codex

Date: 2026-10-10. Owner: Rex. Status: v1. The original design and edit-gate extension were approved by Rex.

## Purpose

Rex: "it's for decision making, not babysit the agent." The system improves
three kinds of agent decision by consulting TypeSafe's Jev model:

1. **Ask versus decide.** The agent asks the user something that the user's
   words or the evidence already settle, or decides something the user owns.
2. **Gut choice.** The agent picks an approach, design, or root-cause theory
   without comparing the options.
3. **Edits before consultation.** Jev judges each proposed edit and classifies
   shell writes before execution. This extension follows Rex's final answers
   in session `44cb74a1-d3a2-4daa-a2a3-2e4b4fd682d9`.

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
| Packaging | One repository, `~/agent-decisions`, written from scratch. Native Claude Code and Codex plugins. |
| Coverage | Claude main agent, Claude subagents, Codex CLI, Codex desktop app. |
| Outage | Fail open, visibly. |
| Anti-cheat | Enforced by hooks, not by instructions. |
| Edit detection | Jev judges each edit and classifies shell writes. |

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

Both hosts run the rewritten command and return the verdict as tool output.
Codex records the completed rewritten command. Claude records the original
command. The hook matches each host's transcript call ID to the ledger.
The completed output must contain the verdict for that ledger action.
An empty output or an inactive stub cannot authorize a later edit.
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
| `agent_decisions/gates.py` | Gate logic: decide, edit, question, plan, stop, ledger guard. |
| `agent_decisions/hook.py` | Hook entry point: payload in, host-specific output out, fail-open. |
| `hooks/hooks.json`, `codex/hooks.json` | Hook registrations for Claude Code and Codex. |
| `skills/deciding/SKILL.md` | When and how to call `ts-decide`. |
| `.claude-plugin/` | Claude Code plugin and local marketplace manifests. |
| `.codex-plugin/` | Native Codex plugin manifest. |

Each hook takes `--host claude` or `--host codex` from its install entry. The
host is never guessed from input fields.
The native `.codex-plugin/plugin.json` selects `codex/hooks.json`, replacing
Codex's default discovery of the Claude hook file.
SessionStart supplies the command and skill paths from the installed package.

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

### 6. Edit gate

Before each edit, Jev evaluates `needs_decision` and `covered` using the
pending tool input, transcript evidence, and this agent's completed decisions.
Shell calls also include `writes_files`. The questions share one request.
An edit with `needs_decision` at least 0.7 requires `covered` at least 0.8.
A shell call enters that rule when `writes_files` is at least 0.7.
Only advisory or binding verdicts backed by this agent's current-turn
transcript calls can cover an edit. An unrelated choice cannot cover it.
An exact user instruction or a routine operation can pass without a choice.
The pending input keeps its first and last 8,000 characters when clipped.
Every edit attempt is judged. A denied retry does not spend an exemption.
Outages still allow the action with a visible warning.

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

Question tools include Claude `AskUserQuestion` and Codex `request_user_input`
and `request_user_input_async`. `made_choice` excludes executing an explicit user instruction. A Stop gate
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

- The native plugin registers PreToolUse, Stop, SubagentStop, and SessionStart.
  Codex resolves `${PLUGIN_ROOT}` to its installed package.
  Its hook list shows only `--host codex` registrations for this package.
- Canonical shell payloads use `tool_name: "Bash"` and `tool_input.command`.
  `apply_patch` also uses `tool_input.command`.
- In code mode, completed `CommandExecution` events provide the shell output
  and the call ID. The outer JavaScript `exec` call is agent input, not evidence.
  A denied shell call produces no completed execution event.
- The deciding skill remains inside the installed plugin. SessionStart supplies
  its absolute path. Native installation needs no global AGENTS.md pointer.
- Codex requires trust for the current hook definition. Changed definitions
  require another review. See `docs/probes.md` for runtime verification.

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

Live: `python3 -B tests/live_cheat.py` runs 16 labelled decision and edit cases
against the real API. The script fails when a result differs from its label.
The observed result was 16 of 16 on 2026-10-10. This sample does not establish
a general catch rate. Host probes check payloads, command rewrites, and native
plugin discovery. See `docs/probes.md` for evidence and limits.

## Rollout

1. Implement; pass the unit tests and the live cheat cases.
2. Claude Code: `claude plugin marketplace add ~/agent-decisions`; install at
   user scope.
3. Codex: install the native plugin and trust its four reviewed hook definitions.
4. Restart sessions from a new login terminal so `TYPESAFE_API_KEY` is set.
5. Review the ledger with Rex after one to two days.

## Not verified

- Manual execution through the Codex desktop GUI. Its app-server hook list
  discovers the native plugin. Runtime execution was tested through the CLI.
- Codex subagent identity and transcript paths in a live subagent call.
- General decision quality and catch rate beyond the 16 labelled API cases.

## Verified by probe (2026-10-10, `docs/probes.md`)

- A Claude `PreToolUse` hook inside a subagent gets `agent_id` and the main
  session's `transcript_path`, not the subagent transcript. The hook derives
  `<session>/subagents/agent-<agent_id>.jsonl` itself.
- At `PreToolUse` time the transcript holds every earlier sequential call but
  not the current call or a parallel sibling. ts-decide runs as its own call.
- Claude applies `updatedInput.command` and keeps the original command in the
  transcript. A deny reason reaches the model.
- Codex CLI 0.162.1 applies `updatedInput.command`. In code mode, the completed
  execution ID matches `PreToolUse.tool_use_id`. The parser reads this event.
- Codex's app-server discovers exactly four native plugin hooks. Each command
  uses the installed package path and `--host codex`.
- The installed Codex v1 plugin completed a real Jev consultation, verified its
  completed verdict in the following edit check, and allowed the covered edit.
- The API accepts `jev-1.13.0`; an 80,000-character state answers in about
  0.4 s.
