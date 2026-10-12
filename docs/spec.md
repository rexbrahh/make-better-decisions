# agent-decisions behavior spec

Date: 2026-10-12. Owner: Rex. Status: v1.1.3 behavior.

## Goal

The hooks help the agent make useful decisions:

1. The agent consults Jev before an unsettled choice.
2. Jev evaluates relevant evidence and returns a pick or a specific next check.
3. The agent uses that direction and continues normal execution.

Exact instructions and routine work proceed directly. Required review blocks unchecked operations after failures. Signed availability policy permits them with warnings.

## Ownership policy

The user decides product behavior and anything destructive, irreversible, external, or paid.

The agent decides engineering details and anything that the user's words or the evidence already settle.

A later user instruction overrides an earlier binding decision. Exact user instructions and routine operations can proceed without an approach decision.

## Evidence contract

The hook builds the evidence bundle from host data. The agent request can add optional `facts`.

Each fact must be a normalized excerpt from captured user text. It must be at least 12 normalized characters. A fact proves only that the user said those words. It does not prove that the quoted claim is true at runtime.

Tool output, source text, parent briefs, and agent text are not valid facts.

The evidence bundle includes these origins and trust labels:

| Field | Origin | Trust label |
|---|---|---|
| `user_messages` | Signed host context and matched prompt-submit records | User-controlled text |
| `brief` | Parent agent brief | `unverified_parent_agent_brief` |
| `tool_calls_newest_first` | Transcript tool observations | `unverified_tool_observation` |
| `source_files` | Hook file reads under `cwd` | `verified_file_observation` |

Each source item includes path, SHA-256 hash, text, origin, trust, and truncation state. Metadata reports candidates, omissions, too-large files, and truncation counts.

The `working_tree_source` role identifies current bytes. Git baseline metadata reports the observed revision, hash, and difference. Unavailable baselines remain explicit. Native command observations include directory, status, and exit code when supplied by the host.

Source-file contents are observations only. They are not user instructions. They are not proof that statements inside the files are true.

The source reader rejects private paths, `.env` files, key files, out-of-root paths, too-large files, binary files, changed files, and symlink swaps. Project `.local` files are allowed unless they are inside the actual state directory.

Evidence is bounded at 80,000 JSON characters. The initial and latest instructions and observations matching cited helper paths precede optional history and project rules. Matching observations retain native directory and trust metadata even when the consultation uses another directory. Hashed source reads remain inside the consultation root. Requested source paths precede project rules and older path candidates.

Requested contracts precede optional observations during source discovery and evidence insertion. The hook validates their relative paths and uses the existing source reader. Candidate discovery remains bounded at 16 paths and source observations at eight files. Requests without contracts retain the existing helper ordering. Printed tool output cannot satisfy the verified contract check.

One text retains at most 16,000 characters plus omission markers. Large sources retain the head, tail, and a window around the most specific matching query term.

## User history

The hook records signed user context. `SessionStart` stores the initial host transcript when available.

`UserPromptSubmit` observations join signed user history only after they match a new human-origin host transcript record. The prompt event alone is not user authority. Claude can emit it for peer or background messages.

Codex `<hook_prompt>` records are automatic feedback. They do not become human history or reset the current turn. Older signed snapshots filter them during repair.

Codex asynchronous question replies do not require a separate prompt-submit event. Each answer must match its earlier completed question call and item index. Quote verification uses the decoded answer, rather than the repeated agent question.

Subagent contexts use main-agent user messages. This lets subagents see signed user history without trusting parent-agent summaries.

## Decisions

Run `ts-decide` as a separate shell call. The command must be the whole shell command.

A request has one decision, two or more neutral options, and optional facts.

Verdicts:

| Verdict | Effect |
|---|---|
| `BINDING` | The subagent must follow the pick. |
| `ADVISORY` | The main agent should follow the pick or state evidence for an override. |
| `PROCEED` | The operation is routine or exactly specified. It does not authorize later choices. |
| `GATHER EVIDENCE` | The agent must inspect or test more, then call again. |
| `ASK USER` | The user owns the choice. Ask the user. |
| `REFUSED` | Repair options or facts, then call again. |
| `PARENT REVIEW` | The worker preference is uncertain. The parent must accept an eligible reviewed option. No receipt is minted. |
| `decision unverified` | Review failed. Required mode grants no permission. Availability mode permits execution with a warning. |

`REFUSED` and `GATHER EVIDENCE` never authorize work. The refusal cap only marks repeated bad requests. It does not create an exemption.

A completed reusable decision needs both a signed ledger entry and the full verdict receipt in the transcript tool output. A matching tool id alone is not enough.

New receipts carry a hook-owned id. Native execution-id changes do not invalidate a complete matching receipt. Legacy receipts retain strict id matching.

Optional scopes permit 32 repository-relative paths and 32 invariants. Each option/invariant pair gets an independent check, capped at 256 checks. Clear invariant violations refuse selection. Uncertain compliance requests evidence. Worker binding requires preference confidence of at least 0.8.

Parent inheritance requires a completed host dispatch with an explicit scoped receipt reference, verified ancestry, and the same repository. The accepted option binds the worker. Accepting a non-picked advisory option requires successful invariant checks and completed evidence references. Nested Claude inheritance is unsupported.

Up to eight contract paths can accompany a request. Contract hashes come from verified current file observations. Missing observations require evidence. Changed, unavailable, or unchecked hashes invalidate approach coverage.

Scoped source snapshots must also match current verified hashes. Source changes, disappearance, or exhausted recheck budgets make that receipt ineligible. Edit, plan, and final coverage share this rule. Routine classification remains separate from approach coverage.

The hook records completed contract reviews as compact signed architectural history. History survives chat retention, remains repository-scoped, and grants no new authority. Rechecked hashes label historical contracts unchanged, changed, unavailable, or unchecked.

Scoped review identity survives neutral question repairs. Unchanged accepted decisions and evidence requests cannot reroll. Refused question wording can be repaired under the same identity. Diagnostics name the affected field and blocking predicates.

The edit, plan, and stop gates can reuse relevant advisory and binding decisions across turns in the same session. Later user instructions override earlier binding decisions.

Decision questions use separate conditions for observed selection grounds, unsupported implementation facts, missing constraints, omitted named alternatives, exact user instructions, and routine work. Code combines these answers. A no-fit answer caused by missing context requests evidence. It does not demand a new option list.

One diagnostic Choice selects the missing request statement from at most 32 candidates. Each candidate retains at most 1,000 characters plus omission markers. The question shares the existing API call. A `GATHER` response quotes the selected statement or explicitly reports that no specific missing fact was identified. The selection does not control authorization. The updated diagnostic contract refreshes older scoped responses once. Unchanged requests then retain the existing reuse rules.

## Gates

### PreToolUse

The hook handles these checks before the tool runs:

- `ts-decide` parsing and verdict generation.
- Direct ledger and verdict-file access denial.
- Question gates and Claude plan checks.
- Edit gates for write tools, shell commands, and MCP tools that can write.
- Opaque stdin checks for shells and interpreters.

Read tools pass without Jev.

### Edit gate

For inputs up to 16,000 JSON characters, the hook sends one Jev request.

For larger inputs, the hook splits the complete tool input into field-path chunks. It keeps field identity, including `old_string`. It derives a noncomment proposed excerpt while retaining originals. All chunks share one three-second deadline.

The hook denies inputs above 256,000 JSON characters.

The edit gate allows exact instructions, tests, formatting, factual logs, and routine corrections using existing helpers or test setup. Regression coverage for selected behavior needs no separate consultation. Coverage uses 0.5 for main-agent advice and 0.8 for workers. The gate denies a new uncovered approach.

The hook denies interactive or opaque commands that can accept future executable stdin. This deny applies while Jev and the hook are healthy.

### Question gate

Question requests are limited to 16 questions and 16,000 JSON characters.
Oversized requests are rejected before Jev.

The hook denies a question when the user's messages already answer it. It also denies a question that the agent should decide.

### Stop gate

The Stop gate checks the final message. It separates `made_choice` from `settled_choice`.

An exact user-settled choice passes at `settled_choice >= 0.8`. A final choice passes only when `uncovered_choice <= 0.2`.

The Stop state highlights the latest user instruction. A later user instruction overrides an earlier binding decision.

The Stop gate can block final questions that the user already answered or that the agent owns. There is no prior-question-tool exemption. There is no active-Stop exemption.

User-question checks read prose outside fenced code. Choice and binding checks retain the full reply, including fenced code. Copied verdict questions do not become questions to the user.

## Review failure policy

Required is the default. Jev errors, timeouts, malformed replies, missing keys, unreadable transcripts, and hook exceptions block unchecked operations. Known read tools remain available.

Only HTTP 400 with the exact `max_tokens_exceeded` validation code permits one retry. Both attempts share the original three-second deadline. The retry shortens tool output and source text with explicit omission markers. Human messages, project rules, contracts, facts, options, scope, pending input, decisions, and observation identities remain intact. Each receipt records the exact redacted state submitted to the API. A second size rejection or any other failure retains required-review denial.

The retry clips optional text to one sixteenth of its current length, with a 128-character minimum and a 1,000-character cap. If the size rejection remains, the diagnostic reports attempt count, request bytes, and aggregate context-section sizes. It omits contents.

Response validation retains finite numeric values, ranges, answer types, choice keys, and the maximum-probability pick. The distribution sum permits the existing one-percent tolerance plus one binary rounding step. Malformed-response diagnostics identify the question and failed check. They omit response contents.

The owner can select availability through signed installation policy. Runtime failures then permit execution with a warning. Healthy denials remain binding. An unverified result never creates an approach receipt.

The launcher denies PreToolUse and blocks Stop/SubagentStop when startup fails. Metadata events show a warning. Partial startup output is discarded. A missing interpreter cannot verify availability policy. The environment disable flag is ignored and reported.

## Ledger

The ledger path is:

```text
~/.local/state/agent-decisions/sessions/<session_id>.jsonl
```

`AGENT_DECISIONS_STATE` overrides the root.

The ledger stores full states, answers, actions, signed user context, installation policy, and verdict text. SessionStart prunes chat records older than 14 days. It retains installation policy and compact architecture history.

Gate records retain timings and API model/usage metadata outside model state. Exact gate caches also match actor, repository, model, thresholds, review contract, and operator policy. Evidence, source hashes, user constraints, and approach receipts remain part of the cache state.

The guard denies tool calls that name the state directory.
Invalid glob syntax in interpreter fragments does not stop path inspection. Literal private paths and valid wildcard matches remain denied. Ordinary tool review still follows.

## Native boundaries

The native installer can build and apply host plans.

The boundary helper is required for the stated tamper protection. Run it from the installed plugin root. If you run it from a checkout, pass `--install-root` with the installed root.

For Codex, it registers an optional permission profile without selecting it as the default. It preserves sandbox and approval settings and wraps configured stdio MCP servers with `agent-decisions-isolate`. For Claude, it writes a shell prefix and wraps matching commands.

Run the helper again after you change stdio MCP or plugin command configuration. A source value of `null` limits automatic refresh. Preserved recipes on repeat runs do not prove update tracking.

The wrapper loads the signed installation recipe from the ledger.

On macOS, the wrapper uses `sandbox-exec`. The macOS deny profile was verified with escalation.

On Linux, the wrapper uses bubblewrap when available. Linux bubblewrap behavior is not runtime verified here.

HTTP MCP servers are not wrapped. Trusted external host services can act outside this repository.

## Verified and unverified boundaries

Verified:

- macOS deny profile behavior through escalation.
- Current install CLI help for `agent-decisions-install` and `agent-decisions-isolate`.
- Current Codex plugin and MCP help for native install commands.

Not verified here:

- Final live hook counts or global activation state.
- Linux bubblewrap runtime behavior.
- Codex GUI identity boundaries.
- New live Codex worker inheritance. Captured native fixtures pass local checks.

Not guaranteed:

- Same-UID privileged processes.
- Operator actions.
- Trusted external host services.
- Claims printed by tools or parent agents.
