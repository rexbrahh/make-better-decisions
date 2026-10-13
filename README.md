# make-better-decisions v1.1

`agent-decisions` adds hook checks for Claude Code and Codex. The hooks ask TypeSafe Jev before an agent chooses an approach, changes files, asks the user a question, or finishes with an uncovered choice.

The goal is useful, enforceable review. Jev evaluates a material choice against observed evidence. Code checks receipt identity, scope, constraints, and failure policy. Exact instructions and routine operations need no invented choice.

Version 1.1.8 preserves Codex and Adrafinil native launchers and installs decision hooks and isolation entrypoints outside the removable plugin cache. It retains the 1.1.7 memory corrections. Required review remains the default. See the [release notes](docs/releases/v1.1.8.md) and [memory verification](docs/probes.md#hook-memory-correction-2026-10-12).

The hook builds the evidence. The agent cannot choose the source set by itself.

## Requirements

- Python 3.9 or later.
- Standard library only.
- A TypeSafe key in `TYPESAFE_API_KEY`, or in `~/.config/sops-nix/secrets/typesafe_api_key`.
- Local Claude Code or local Codex on macOS or Linux.

Windows is not supported. The ledger uses POSIX file locks.

## Install

The commands below match the current `--help` output checked on 2026-10-11.

### Claude Code

```sh
/plugin install agent-decisions --marketplace rexbrahh/make-better-decisions
```

For older Claude Code versions, add the marketplace first:

```sh
/plugin marketplace add rexbrahh/make-better-decisions
/plugin install agent-decisions@make-better-decisions
```

Restart Claude Code from a shell that has the TypeSafe key.

### Codex native plugin

```sh
codex plugin marketplace add rexbrahh/make-better-decisions
codex plugin add agent-decisions@make-better-decisions
```

For a local checkout, pass the checkout path as the marketplace source.

After the native plugin loads, trust the five hooks:

- `PreToolUse`
- `Stop`
- `SubagentStop`
- `SessionStart`
- `UserPromptSubmit`

Do not keep a legacy manual hook entry for this plugin. Keep other plugins' hooks.

### Boundary helper

The boundary helper is required for the stated tamper protection. Install the native plugin first. Then run the helper from the installed plugin root. If you run it from a checkout, pass `--install-root` with the installed root.

```sh
python3 bin/agent-decisions-install --host codex --install-root /path/to/installed/agent-decisions
python3 bin/agent-decisions-install --host codex --install-root /path/to/installed/agent-decisions --apply
python3 bin/agent-decisions-install --host claude --install-root /path/to/installed/agent-decisions
python3 bin/agent-decisions-install --host claude --install-root /path/to/installed/agent-decisions --apply
```

Use per-host commands when you need separate evidence or rollback. The protected authority cache is sealed while the development checkout stays editable.

The Codex helper registers an optional isolation profile. It preserves the default sandbox, approval policy, and reviewer. Select the profile explicitly to apply its native filesystem restrictions.

Run the helper again after each plugin upgrade or stdio MCP configuration change. The helper copies verified runtime files into `~/.local/share/agent-decisions/runtimes/` and atomically activates `current`. It retains earlier bundles for running processes. Plugin cache removal does not remove these entrypoints. Cache updates alone do not activate new runtime code.

Hooks use the protected runtime after installation. Before installation, they use the plugin entrypoint. A broken installed runtime reports a startup failure. The runtime namespace, pointer, and bundles are write-protected by the configured boundary. Required decision review remains active.

The Codex helper preserves native `codex_app` and `adrafinil` registrations. It restores legacy wrapped registrations from signed original recipes. Other stdio servers retain isolation. A source value of `null` limits automatic refresh. Repeat runs preserve signed recipes, so they do not prove that every upstream command change was tracked.

Useful verified help commands:

```sh
python3 bin/agent-decisions-install --help
python3 bin/agent-decisions-isolate --help
python3 bin/agent-decisions-isolate exec --help
codex plugin marketplace add --help
codex plugin add --help
codex mcp list --help
```

`agent-decisions-shell-prefix` is an execution wrapper. It is not a help-style CLI.

## Operation

Hook context reads transcript and signed ledger records incrementally. It retains at most 16 optional edit, question, plan, or Stop cache payloads. An older cache miss requires fresh review. Complete decision entries, user history, transcript identity checks, and verdict receipts remain intact. Memory use still depends on retained authority, tool text, and the largest individual record.

Run `ts-decide` only as its own shell call. Read the verdict before the dependent call.

`facts` is optional. Each fact must be a normalized excerpt from captured user text. It must be at least 12 normalized characters. A fact proves only that the user said those words. It does not prove that the quoted claim is true at runtime.

The hook supplies these evidence groups:

- User messages from signed host context.
- Tool observations and source-file observations with origins and trust labels.
- Parent briefs with `unverified_parent_agent_brief` trust.
- Metadata for included, omitted, truncated, and too-large evidence.

`UserPromptSubmit` is not user authority by itself. The hook adds it to signed user history only after it matches a new human-origin host transcript record. This matters because Claude can emit prompt-submit events for peer or background messages.

Codex question replies can arrive during a running turn without `UserPromptSubmit`. The hook captures those host records after matching every reply to its earlier completed question call, question text, and item index. It preserves the saved history when the host transcript is replaced. Quote verification decodes the answers, including quotes and line breaks. The agent's repeated question text is not a user quotation.

Source-file contents are observations. They are not proof that statements inside those files are true.

Current source has the `working_tree_source` role. Git baseline metadata records the revision, content hash, and whether current bytes differ. An unavailable or oversized baseline stays explicit. Native Codex command observations retain directory, status, and exit code. Printed success text cannot establish a passing check.

Cited helper observations precede optional history and project rule files within the existing evidence budget. Captured reads from another worktree retain their native directory and trust label. The hashed source reader retains its current directory boundary.

The bundle keeps the initial and latest user instructions, the requested helper, and the two newest tool observations before optional history. Older observations use the remaining space.

Source discovery prioritizes requested paths, then project rule files, then other observed paths. It stops at 16 candidate probes and bounds each scanned text. Large sources retain the head, tail, and a window around the most specific matching query term. If discovery stops early, `source_files_omitted_is_lower_bound` marks the omission count as a lower bound.

Jev questions check one condition at a time. Code combines the answers. A decision needs an observed selection basis and no missing required fact or constraint. This follows [TypeSafe's atomic-question guidance](https://docs.typesafe.ai/introduction#atomic-questions-composed-in-code).

An observed interface or data-flow pattern can provide a selection basis when the chosen option follows it. Main-agent advice uses a 0.5 basis threshold when agent ownership is at least 0.7. Worker bindings retain 0.8. Missing facts, constraints, options, and user ownership still gate the result.

Routine fixes that reuse existing helpers, test setup, or regression patterns need no additional consultation. A selected approach covers its implementation and routine tests. The edit gate uses 0.5 for main-agent advisory coverage and 0.8 for worker coverage.

A completed decision must have both records:

1. A signed ledger entry.
2. The full verdict receipt in the transcript tool output.

New reusable verdicts include a signed `review_...` receipt id. The id survives execution-id changes. Legacy verdicts retain their strict execution-id check.

### Scoped review and parent acceptance

Add optional `scope` and `contracts` fields to a decision request:

```json
{
  "decision": "Which lease representation should parsing use?",
  "options": [
    {"id": "shared", "description": "Reference-counted lease"},
    {"id": "owned", "description": "Single-owner lease"}
  ],
  "scope": {"paths": ["src/parser.py"], "invariants": ["Retain active leases until parsing ends."]},
  "contracts": ["docs/lease-contract.md"]
}
```

Paths are repository-relative files or directory prefixes ending in `/`. Globs, absolute paths, and parent traversal are rejected. Scope permits at most 32 paths and 32 invariants. A request permits at most 256 option/invariant checks and eight contract files.

Jev checks each option against each invariant. A clear violation refuses the pick. Uncertain support requests evidence. Worker binding also requires preference confidence of at least 0.8. A weaker worker preference produces `PARENT REVIEW`, without a receipt. Main-agent preferences remain advisory. [TypeSafe confidence measures the distribution](https://docs.typesafe.ai/confidence), so it does not certify correctness.

For approach selection, the questions assess compatibility with observed constraints. New implementation and execution checks can remain future work. A proposed check is not completed proof. Necessary current prerequisites still require observations. If GATHER identifies no missing fact, inspect its displayed blockers and invariant text. Do not repeat a completed comparison.

A parent assigns a completed scoped receipt in its dispatch message:

```text
Decision receipt: review_<id from the completed verdict>
Accepted option: shared
Evidence refs: <completed source or test call ids, separated by commas>
```

The chosen option must belong to the reviewed options. Accepting the advisory pick needs no evidence-reference marker. Accepting another option requires completed evidence references and successful invariant checks for that option. These references establish observed data, not proof that assertions in the data are true.

The hook checks native ancestry, completed dispatch, repository identity, the signed entry, and the full parent receipt. The accepted approach binds the worker. A matching filename alone grants no permission. Codex uses captured native metadata. Claude supports completed main-thread Agent/Task dispatches. Nested Claude ancestry remains unsupported.

Unchanged scoped reviews reuse their result. Question rephrasing cannot reroll an accepted pick or an unresolved evidence request. A neutral repair of a refused question can receive another check while retaining its decision identity. Refusal diagnostics name the wording field. Evidence diagnostics show the blocking values and quote the request statement that needs evidence. Jev selects that statement from bounded candidates in the same API call. If no candidate identifies the missing fact, the response says so. This diagnostic does not change the verdict.

Completed reviews with contract references produce compact signed architectural history. History is repository-scoped background data and survives chat retention. It grants no fresh write permission. Changed, unavailable, or unchecked contract hashes make the corresponding approach receipt ineligible.

Scoped receipts also require fresh hashes for retained source observations, even without declared contracts. Source changes can require fresh review for later material choices. Routine and exact operations still use the existing classifier. Hash checks remain bounded, so unchecked snapshots grant no coverage.

A later user instruction overrides an earlier binding decision. The Stop gate highlights the latest user instruction when it checks final text.

`REFUSED` and `GATHER EVIDENCE` do not authorize work. There is no retry-cap exemption.

A `PROCEED` verdict means Jev judged the operation routine or exactly specified. It does not create a reusable approach decision.

## Gates

- `PreToolUse` handles `ts-decide`, edit gates, question tools, plan checks, and ledger guards.
- It also handles shell-write classification, MCP write gates, and opaque stdin checks.
- `Stop` and `SubagentStop` block final messages that report an uncovered choice.
- They also block final questions that the user already answered or that the agent owns.
- `SessionStart` and `UserPromptSubmit` support pruning, path context, and signed user history.

There is no prior-question-tool exemption. There is no active-Stop exemption.

Stop accepts exact user-settled choices at `settled_choice >= 0.8`. It accepts a final choice only when `uncovered_choice <= 0.2`.

For tool inputs up to 16,000 characters, the edit gate sends one API request. Larger complete fields are split into field-path chunks. The chunks keep original fields and proposed fields distinct. They share one three-second deadline. Inputs above 256,000 JSON characters are denied.

Opaque commands that can accept future executable stdin are denied while the hook is healthy. Submit the complete command or code in one new shell call.

## Review failures and recovery

Required review is the default in this implementation. API errors, unreadable context, and hook exceptions block unchecked actions, questions, plans, and final reviews. Known read tools continue without Jev. Unknown tools and shell commands require classification.

An HTTP 400 with `max_tokens_exceeded` triggers one retry within the original three-second deadline. The retry shortens historical tool input, output, and source excerpts. Historical inputs retain omission markers and original lengths. It preserves human messages, rule files, contracts, current pending input, and completed decisions. The receipt records the exact redacted state sent to Jev. A failed retry still blocks execution. Other validation failures identify the question and failed check without displaying response contents.

The retry limits optional observation text to one sixteenth of its previous length, with a 128-character minimum and a 1,000-character cap. A remaining size rejection reports aggregate context-section sizes and attempt count. It does not display their contents.

The owner can select availability mode through the protected installer. Preview the signed policy before applying it:

```sh
python3 bin/agent-decisions-install --host codex --review-mode required --install-root /path/to/installed/agent-decisions
python3 bin/agent-decisions-install --host codex --review-mode availability --install-root /path/to/installed/agent-decisions
```

An operator adds `--apply` to the chosen command to change host configuration and signed policy. Availability permits actions after runtime review failures and shows a warning. It weakens enforcement. Healthy denials remain binding in both modes.

Startup failures block reviewed operations because a missing interpreter cannot read signed policy. The launcher discards partial startup output and preserves healthy denial JSON. `AGENT_DECISIONS_OFF=1` is ignored and reported. Tool input and parent briefs cannot change review mode.

For recovery, restore the key, service connection, readable host transcript, or installed package named in the diagnostic. Retry the original operation. An unverified result grants no reusable approach receipt. If service recovery is unavailable, the owner can explicitly apply availability policy.

## State

Ledger files are in:

```text
~/.local/state/agent-decisions/sessions/<session>.jsonl
```

Set `AGENT_DECISIONS_STATE` to override the state root during operator setup. The guard denies tool calls that name the state directory. Session records expire after 14 days. Signed installation policy and architecture history remain available.

Signed gate records retain context timings, initial and final request sizes, attempt count, API elapsed time, model, and numeric usage when returned. Metrics stay outside model state and cache fingerprints. Gate reuse requires identical evidence, decisions, actor, repository, model, thresholds, review contract, and operator policy. It does not infer permission for a new write.

## Security boundaries

The macOS native deny profile was verified with escalation. It denies reads and writes to the agent-decisions state root. It denies writes to protected host state and installed plugin code.

Protection includes the durable runtime namespace, this plugin's cached version namespace, and existing Codex profile files. Stdio launchers use Python isolated mode before reading signed recipes.

Linux uses bubblewrap when available. Linux bubblewrap behavior is not runtime verified in this repository.

The boundary helpers wrap third-party stdio MCP servers. Codex and Adrafinil retain their native launchers. The helpers do not wrap HTTP MCP servers or trusted external services.

These hooks are not a universal proof boundary. They do not protect against a same-UID privileged process, an operator, or a trusted external host service that acts outside the hook path.

Captured native Codex ancestry and Claude dispatch fixtures pass parser and receipt tests. New live GUI activation and worker inheritance remain unverified.

## Tests

Release verification uses the observed peer workflows. A successful consultation must give useful direction, and ordinary execution must continue.

| Check | Pass condition |
|---|---|
| Honest consultation | A native peer makes a real API call before an unsettled choice and reports the actual verdict. |
| SDK answer and direction | The exact host question reply is accepted. The save/export choice gets a usable verdict. |
| E2E evidence follow-up | The requested helper reaches Jev. Reading it resolves the request or produces a different justified next step. |
| Routine work | Reads, diagnostics, tests, and exact instructions proceed without unnecessary consultation. |
| API failure | Required mode denies unchecked actions. Signed availability mode permits them with a warning. |

Run unit tests with:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -t . -v
```

Run live decision and edit cases with a real TypeSafe key:

```sh
python3 -B probe/live_qa.py --output /private/tmp/agent-decisions-live-qa.jsonl
python3 -B tests/live_cheat.py
python3 -B tests/live_ergonomics.py
python3 -B tests/live_scoped_review.py --output /private/tmp/agent-decisions-scoped-live.json
```

Measure local context overhead without an API call:

```sh
python3 -B tests/benchmark_context.py --host codex --transcript /path/to/rollout.jsonl --cwd /path/to/repo
```

The benchmark emits timings, evidence size, and a hash. Use `--through-bytes` to replay a fixed transcript prefix. It emits no transcript text.
