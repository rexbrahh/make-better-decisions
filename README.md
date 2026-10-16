# make-better-decisions v2.2

`agent-decisions` supplements Claude Code and Codex with selective decision review. Agents consult the selected reviewer before a new material choice. Hooks enforce review on declared edit and patch tools, filesystem write/edit tools, and plan acceptance.

The goal is useful, enforceable review. The reviewer evaluates a material choice against observed evidence. Code checks receipt identity, scope, constraints, and failure policy. Exact instructions and routine operations need no invented choice.

Version 2.2.0 adds optional SubQ Snap review with larger evidence and bounded question batches. TypeSafe Jev remains the default. Snap is a preview provider and needs further stability qualification before default promotion. See the [release notes](docs/releases/v2.2.0.md).

Version 2.1.3 reports the source root used for unavailable contract files. See the [release notes](docs/releases/v2.1.3.md) and [QA record](docs/validation/v2.1.3-qa-2026-10-15.md). Earlier releases correct [PROCEED reuse](docs/releases/v2.1.2.md), [installer credential diagnostics](docs/releases/v2.1.1.md) and [installation and compatibility defects](docs/validation/v2.1.0-campaign-2026-10-15.md).

Version 2.0.0 replaces the Python engine with one Go binary, `agent-decisions`. The hook output, ledger rows and seals stay byte-identical to version 1.2.8. On 6,206 hook calls replayed from real transcripts, the Go engine took 1 min 40 s and the Python engine took 2 min 5 s. Ordinary tools remain independent of Jev. Required review remains the default on the declared sensitive surface. See the [release notes](docs/releases/v2.0.0.md).

Version 2.0.1 simplifies CLI presentation, preserves release assets after a failed build, and includes the Linux error-text correction. See the [maintenance notes](docs/releases/v2.0.1.md) and [QA evidence](docs/validation/go-qa-2026-10-15.md).

The hook builds the evidence. The agent cannot choose the source set by itself.

## Requirements

- macOS on Apple silicon (darwin-arm64) or Linux on x86-64 (linux-amd64).
- `curl`, and `sha256sum` or `shasum`, for the installer. Python is not necessary.
- A key for the selected provider. Jev uses `TYPESAFE_API_KEY` or `~/.config/sops-nix/secrets/typesafe_api_key`.
- Local Claude Code or local Codex on macOS or Linux.

Windows is not supported. The ledger uses POSIX file locks.

## Install

The commands below match the installed CLI help checked on 2026-10-15.

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

After the native plugin loads, trust the three hooks:

- `PreToolUse`
- `SessionStart`
- `UserPromptSubmit`

Do not keep a legacy manual hook entry for this plugin. Keep other plugins' hooks.

### Boundary helper

The boundary helper is required for the stated tamper protection. Install the native plugin first. Then run the helper from the installed plugin root. If you run it from a checkout, pass `--install-root` with the installed root.

```sh
bin/agent-decisions-install --host codex --install-root /path/to/installed/agent-decisions
bin/agent-decisions-install --host codex --install-root /path/to/installed/agent-decisions --apply
bin/agent-decisions-install --host claude --install-root /path/to/installed/agent-decisions
bin/agent-decisions-install --host claude --install-root /path/to/installed/agent-decisions --apply
```

The helper downloads the release binary for this platform and the version in `.claude-plugin/plugin.json`. It compares the SHA-256 of the binary with the pin in `bin/agent-decisions.sha256`. If the hashes are different, the helper stops and installs nothing. To use a local binary, set `AGENT_DECISIONS_BINARY` to its path. The helper checks a local binary against the same pin.

For Claude, `CLAUDE_CONFIG_DIR` selects the settings and plugin registry directory. The default is `~/.claude`. An explicit `--claude-settings` overrides the settings path. Protection still covers all existing Claude configuration roots.

Use per-host commands when you need separate evidence or rollback. The protected authority cache is sealed while the development checkout stays editable.

The Codex helper registers an optional isolation profile. It preserves the default sandbox, approval policy, and reviewer. Select the profile explicitly to apply its native filesystem restrictions.

If an automatically detected Codex socket fails initialization, the installer retries through local stdio.
An explicit socket stays authoritative. The installer never retries after it sends the requested configuration operation.

Run the helper again after each plugin upgrade or stdio MCP configuration change. The helper copies verified runtime files into `~/.local/share/agent-decisions/runtimes/` and atomically activates `current`. It retains earlier bundles for running processes. Plugin cache removal does not remove these entrypoints. Cache updates alone do not activate new runtime code.

Hooks run the binary in the installed runtime. Before installation, hooks deny reviewed tool calls and report that the engine is not installed. A runtime from a release before 2.0.0 runs its Python hook until you run the helper again. A broken installed runtime reports a startup failure. The runtime namespace, pointer, and bundles are write-protected by the configured boundary. Required decision review remains active.

The Codex helper preserves native `codex_app`, `adrafinil`, `node_repl`, and `cua_repl` registrations. It also preserves the bundled computer-history, computer-use, event-stream, mcp-search, and messages launchers. It restores legacy wrapped registrations from signed original recipes. Other stdio servers retain isolation. A source value of `null` limits automatic refresh. Repeat runs preserve signed recipes, so they do not prove that every upstream command change was tracked.

Useful verified help commands:

```sh
bin/agent-decisions-install --help
bin/agent-decisions-isolate --help
bin/agent-decisions-isolate exec --help
codex plugin marketplace add --help
codex plugin add --help
codex mcp list --help
```

`agent-decisions-shell-prefix` is an execution wrapper. It is not a help-style CLI.

### Optional SubQ Snap provider

Snap uses `SUBQ_API_KEY` or `~/.config/sops-nix/secrets/subq_api_key`. Make the key available to the host process before starting it.
The installer stores only the provider name in signed host policy. It does not store the API key.

Preview the selection, then apply it for each intended host:

```sh
bin/agent-decisions-install --host codex --review-provider subq --install-root /path/to/installed/agent-decisions
bin/agent-decisions-install --host codex --review-provider subq --install-root /path/to/installed/agent-decisions --apply
bin/agent-decisions-install --host claude --review-provider subq --install-root /path/to/installed/agent-decisions --apply
```

Use `--review-provider jev` to restore Jev. An omitted flag preserves the existing selection.
An unknown signed selection blocks reviewed operations. An explicit valid installer selection repairs it.
Tool input and environment variables cannot select the provider. Provider changes require fresh material review.

| Limit | Jev | SubQ Snap |
| --- | --- | --- |
| Model | `jev-1.13.0` | `subq-snap-preview` |
| Evidence budget | 80,000 serialized characters | 800,000 serialized characters |
| Text excerpt | 16,000 characters | 160,000 characters |
| Review deadline | 3 seconds | 10 seconds across all fragments and batches |
| Questions per API request | Existing Jev request | At most 16, with at most four requests running |

Snap sends each batch the same scrubbed state. Every batch must pass validation before any answer reaches a gate.
Snap requests stop at 2 MiB and successful responses stop at 1 MiB. Redirects and provider fallback are rejected.
Snap does not retry with shorter context. A character budget does not guarantee that input fits the model token limit.
Existing source-file counts, source-file sizes, tool-input limits, redaction, authority checks and failure modes remain active.
The [provider design](docs/superpowers/specs/2026-10-15-subq-provider-design.md) and [QA record](docs/validation/v2.2.0-qa-2026-10-16.md) state qualification limits.

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

Claude question-box answers also arrive without `UserPromptSubmit`. The hook accepts an answer only when Claude Code records it as the user's own pick (`permissionDecision` source `user_temporary`), and only for a question that the earlier call asked. Agents can quote the selected label or the text that the user typed. A hook or an SDK callback can also fill in answers, and the hook does not accept those as user text.

Claude stores a message that the user types while the agent works as a `queued_command` record. The hook reads it as user text only when the record has a human origin. Peer messages and task notifications use the same record type, and the hook ignores them. A forwarded side-agent note looks the same as a typed message, so agents can quote it.

Source-file contents are observations. They are not proof that statements inside those files are true.

Current source has the `working_tree_source` role. Git baseline metadata records the revision, content hash, and whether current bytes differ. An unavailable or oversized baseline stays explicit. Native Codex command observations retain directory, status, and exit code. Printed success text cannot establish a passing check.

Cited helper observations precede optional history and project rule files within the existing evidence budget. Captured reads from another worktree retain their native directory and trust label. The hashed source reader retains its current directory boundary.

The bundle keeps the initial and latest user instructions, the requested helper, and the two newest tool observations before optional history. Older observations use the remaining space.

Source discovery prioritizes requested paths, then project rule files, then other observed paths. It stops at 16 candidate probes and bounds each scanned text. Large sources retain the head, tail, and a window around the most specific matching query term. If discovery stops early, `source_files_omitted_is_lower_bound` marks the omission count as a lower bound.

Reviewer questions check one condition at a time. Code combines the answers. A decision needs an observed selection basis and no missing required fact or constraint. This follows [TypeSafe's atomic-question guidance](https://docs.typesafe.ai/introduction#atomic-questions-composed-in-code).

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

The reviewer checks each option against each invariant. A clear violation refuses the pick. Uncertain support requests evidence. Worker binding also requires preference confidence of at least 0.8. A weaker worker preference produces `PARENT REVIEW`, without a receipt. Main-agent preferences remain advisory. [TypeSafe confidence measures the distribution](https://docs.typesafe.ai/confidence), so it does not certify correctness.

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

A later user instruction overrides an earlier binding decision.

`REFUSED` and `GATHER EVIDENCE` do not authorize work. There is no retry-cap exemption.

A `PROCEED` verdict means the reviewer judged the operation routine or exactly specified. It does not create a reusable approach decision.

## Gates

- `PreToolUse` handles explicit `ts-decide`, declared edit tools, plan acceptance, and the local private-state guard.
- Ordinary shell commands, peer messaging, worker dispatch, questions, and unknown tools follow normal host controls. They need no transcript context or Jev API call.
- Automatic Stop and SubagentStop review is removed. Previously loaded completion callbacks return without blocking.
- `SessionStart` and `UserPromptSubmit` support pruning, path context, and signed user history.

For tool inputs up to 16,000 characters, the edit gate makes one review call. Larger complete fields are split into field-path chunks. The chunks keep original fields and proposed fields distinct. They share the selected provider's deadline. Snap can split each call into question batches. Inputs above 256,000 JSON characters are denied.

Submit complete commands or code in one shell call. The deciding workflow does not permit future executable stdin.

## Review failures and recovery

Required review is the default for explicit consultations and declared sensitive tools. Review failures block those operations. Other tools and completion continue without Jev. File changes through other interfaces remain subject to host permissions and approvals.

A request schema error produces no verdict or receipt. Independent, authorized factual reads and checks can continue through ordinary tools.
Declared edit tools still require available review, including for routine corrections.

For Jev, HTTP 400 with `max_tokens_exceeded` triggers one retry within the original three-second deadline. The retry shortens historical tool input, output, and source excerpts. Historical inputs retain omission markers and original lengths. It preserves human messages, rule files, contracts, current pending input, and completed decisions. The receipt records the exact redacted state sent to the reviewer. A failed retry still blocks execution. Other validation failures identify the question and failed check without displaying response contents.

The retry limits optional observation text to one sixteenth of its previous length, with a 128-character minimum and a 1,000-character cap. A remaining size rejection reports aggregate context-section sizes and attempt count. It does not display their contents.

The owner can select availability mode through the protected installer. Preview the signed policy before applying it:

```sh
bin/agent-decisions-install --host codex --review-mode required --install-root /path/to/installed/agent-decisions
bin/agent-decisions-install --host codex --review-mode availability --install-root /path/to/installed/agent-decisions
```

An operator adds `--apply` to the chosen command to change host configuration and signed policy. Availability permits actions after runtime review failures and shows a warning. It weakens enforcement. Healthy denials remain binding in both modes.

Startup failures block reviewed operations because a missing interpreter cannot read signed policy. The launcher discards partial startup output and preserves healthy denial JSON. `AGENT_DECISIONS_OFF=1` is ignored and reported. Tool input and parent briefs cannot change review mode.

The owner can grant a standing exception to one canonical session and its native workers. Preview the protected policy, then add `--apply` to activate it:

```sh
bin/agent-decisions-install --host codex --session-review-exception SESSION_ID
bin/agent-decisions-install --host codex --clear-session-review-exception SESSION_ID
```

Exceptions persist across plugin updates until explicit owner revocation. They bypass sensitive execution review, including a false semantic denial. They do not create a Jev receipt. Explicit consultations, private-state protection, native identity checks, and host approvals remain active.

For recovery, restore the key, service connection, readable host transcript, or installed package named in the diagnostic. Retry the original operation. An unverified result grants no reusable approach receipt. If service recovery is unavailable, the owner can explicitly apply availability policy.

### Inspecting Codex CLI consultation records

Codex CLI `command_execution` events can show a command after a hook applies `updatedInput`.
This plugin replaces an intercepted `ts-decide` command with `printf '%s\n' <verdict>` to return the verdict through the shell.
A `printf` event alone does not establish a simulated or missing consultation.

1. Inspect the original native `custom_tool_call.input`. Classic `function_call` records use `arguments`. Code-mode input can contain a nested `tools.exec_command` request.
2. Correlate that request with execution events and returned output. Use available call IDs and timestamps.
3. Read the verdict and retain its scope. A rewritten command alone proves neither a backend API request nor a reusable receipt.

The [hook rewrite](internal/hook/hook.go), [transcript recognition](internal/transcript/transcript.go), and [probes A4–A6](docs/probes.md) describe this behavior.
`PROCEED` permits the routine or exact instruction. It does not approve a material approach.

## State

Ledger files are in:

```text
~/.local/state/agent-decisions/sessions/<session>.jsonl
```

Set `AGENT_DECISIONS_STATE` to override the state root during operator setup. The guard denies tool calls that name the state directory, including glob, `~`, `$HOME`, and `{a,b}` spellings in shell commands. Session records expire after 14 days. Signed installation policy and architecture history remain available.

Signed gate records retain context timings, initial and final request sizes, attempt count, API elapsed time, model, and numeric usage when returned. Snap also records provider, batch count, request sizes and summed usage. Metrics stay outside model state and cache fingerprints. Gate reuse requires identical evidence, decisions, actor, repository, provider, model, context profile, thresholds, review contract, and operator policy. Legacy receipts qualify only for the original Jev identity. It does not infer permission for a new write.

## Security boundaries

The macOS native deny profile was verified with escalation. It denies reads and writes to the agent-decisions state root. It denies writes to protected host state and installed plugin code.

Protection includes the durable runtime namespace, this plugin's cached version namespace, and existing Codex profile files. Stdio launchers run the binary of the installed runtime before they read signed recipes.

Linux uses bubblewrap when available. Linux amd64 isolation tests passed in a privileged Ubuntu 24.04 container on 2026-10-15. The tests pre-created protected host paths. A missing protected path still causes exit 78. See the [QA evidence and limits](docs/validation/go-qa-2026-10-15.md).

The boundary helpers wrap third-party stdio MCP servers. Codex and Adrafinil retain their native launchers. The helpers do not wrap HTTP MCP servers or trusted external services.

These hooks are not a universal proof boundary. They do not protect against a same-UID privileged process, an operator, or a trusted external host service that acts outside the hook path.

Captured native Codex ancestry and Claude dispatch fixtures pass parser and receipt tests.
Native Codex workers with encrypted briefs use the reserved receipt task name in the [assignment instructions](skills/deciding/SKILL.md).
The [native worker validation record](docs/validation/native-worker-inheritance-2026-10-16.md) records the live receipt and coverage checks.

## Tests

Release verification uses the observed peer workflows. A successful consultation must give useful direction, and ordinary execution must continue.

Provider qualification covers [copied approvals](docs/validation/copied-approval-qualification-2026-10-16.md) and [response-body cancellation](docs/validation/subq-body-cancellation-2026-10-16.md).
The [native receipt coverage record](docs/validation/native-receipt-coverage-2026-10-16.md) separates completed checks from host authentication limits.
Native checks inherit `CLAUDE_CONFIG_DIR`. Set it to the authenticated configuration root used for installation.
Check `claude auth status` in that same environment. Linked settings do not prove shared authentication.

| Check | Pass condition |
|---|---|
| Honest consultation | A native peer makes a real API call before an unsettled choice and reports the actual verdict. |
| SDK answer and direction | The exact host question reply is accepted. The save/export choice gets a usable verdict. |
| E2E evidence follow-up | The requested helper reaches Jev. Reading it resolves the request or produces a different justified next step. |
| Routine work | Reads, diagnostics, tests, and exact instructions proceed without unnecessary consultation. |
| API failure | Required sensitive operations stop. Ordinary tools and completion continue. Owner session exceptions and signed availability policy control explicit exceptions. |

Build and test with Go 1.27.2:

```sh
go test ./...
```

The oracle tests compare the Go engine with the Python engine byte for byte. They need CPython 3.15 and a checkout of the tag `python-final`:

```sh
git worktree add "$TMPDIR/agent-decisions-python" python-final
AGENT_DECISIONS_PYTHON=/opt/homebrew/bin/python3 AGENT_DECISIONS_PYTHON_SOURCE="$TMPDIR/agent-decisions-python" go test ./...
```

To replay real transcripts through both engines, also set `AGENT_DECISIONS_REAL_TRANSCRIPTS=1` and run `TestReplayRealTranscripts` in `internal/hook` with `-timeout 0`. The test logs only counts, tool names and line numbers.

Build the release binaries and their pins:

```sh
scripts/build-release
git diff --exit-code bin/agent-decisions.sha256
```

The build is reproducible. A second build of the same commit gives the same SHA-256 values.
