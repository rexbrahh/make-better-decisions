# make-better-decisions v1.0.1

`agent-decisions` adds hook checks for Claude Code and Codex. The hooks ask TypeSafe Jev before an agent chooses an approach, changes files, asks the user a question, or finishes with an uncovered choice.

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

Run the helper again after you change stdio MCP or plugin command configuration. A source value of `null` limits automatic refresh. Repeat runs preserve signed recipes, so do not treat them as proof that every upstream command change was tracked.

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

Run `ts-decide` only as its own shell call. Read the verdict before the dependent call.

`facts` is optional. Each fact must be a normalized excerpt from captured user text. It must be at least 12 normalized characters. A fact proves only that the user said those words. It does not prove that the quoted claim is true at runtime.

The hook supplies these evidence groups:

- User messages from signed host context.
- Tool observations and source-file observations with origins and trust labels.
- Parent briefs with `unverified_parent_agent_brief` trust.
- Metadata for included, omitted, truncated, and too-large evidence.

`UserPromptSubmit` is not user authority by itself. The hook adds it to signed user history only after it matches a new human-origin host transcript record. This matters because Claude can emit prompt-submit events for peer or background messages.

Source-file contents are observations. They are not proof that statements inside those files are true.

A completed decision must have both records:

1. A signed ledger entry.
2. The full verdict receipt in the transcript tool output.

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

## Fail-open behavior

Rex approved visible fail-open. If Jev, the transcript, or the hook fails, the hook allows the action and shows a warning. `ts-decide` prints `decision unverified` with the reason.

Use `AGENT_DECISIONS_OFF=1` only for approved maintenance.

## State

Ledger files are in:

```text
~/.local/state/agent-decisions/sessions/<session>.jsonl
```

Set `AGENT_DECISIONS_STATE` to override the state root. The guard denies tool calls that name the state directory. The ledger keeps entries for 14 days.

## Security boundaries

The macOS native deny profile was verified with escalation. It denies reads and writes to the agent-decisions state root. It denies writes to protected host state and installed plugin code.

Linux uses bubblewrap when available. Linux bubblewrap behavior is not runtime verified in this repository.

The boundary helpers wrap stdio MCP servers. They do not wrap HTTP MCP servers or trusted external services.

These hooks are not a universal proof boundary. They do not protect against a same-UID privileged process, an operator, or a trusted external host service that acts outside the hook path.

Codex GUI and Codex subagent identity boundaries are not verified.

## Tests

Run unit tests with:

```sh
PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -t . -v
```

Run live decision and edit cases with a real TypeSafe key:

```sh
python3 -B tests/live_cheat.py
python3 -B tests/live_ergonomics.py
```
