# make-better-decisions v1

Hooks that make Claude Code and Codex consult TypeSafe's Jev model before
an agent picks between approaches, edits files, or asks the user to decide.
The hook, not the agent, gathers the evidence from the session transcript, so
the agent cannot hand Jev a one-sided picture. Design: `docs/spec.md`.

## Requirements

- Python 3.9+ (stdlib only)
- A TypeSafe API key, in `TYPESAFE_API_KEY` or in
  `~/.config/sops-nix/secrets/typesafe_api_key`

## Install (Claude Code)

```
/plugin install agent-decisions --marketplace rexbrahh/make-better-decisions
```

On Claude Code older than 2.1.275, run
`/plugin marketplace add rexbrahh/make-better-decisions` and then
`/plugin install agent-decisions@make-better-decisions`.

Restart Claude Code from a shell that has the key. The plugin adds the
`deciding` skill, the `ts-decide` command, and PreToolUse, Stop, SubagentStop
and SessionStart hooks.

## Install (Codex CLI and desktop)

```sh
codex plugin marketplace add rexbrahh/make-better-decisions
codex plugin add agent-decisions@make-better-decisions
```

For a local checkout, use its absolute path as the marketplace source.
The native Codex manifest selects `codex/hooks.json`. Its hook paths resolve
from the installed package. It does not also load the Claude hook file.

Review the four plugin hooks in `/hooks`. Codex skips untrusted hooks.
Restart the session after installation. The SessionStart hook provides the
absolute `ts-decide` path, so installation needs no PATH or AGENTS.md edit.
Keep one registration: remove legacy manual entries for this plugin before
enabling the native package. Preserve other plugins' hooks.

Supported hosts are local Claude Code and local Codex sessions on macOS or
Linux with Python 3.9+. Windows is not supported because the ledger uses
POSIX file locks. See `docs/probes.md` for the verified host versions and limits.

## Operation

- Jev judges each edit before execution. It also classifies shell writes.
  A change that selects an implementation needs a completed, relevant
  `ts-decide` verdict in this agent's current turn. An unrelated verdict
  cannot authorize the change. An exact user instruction can pass directly.
- Run `ts-decide` in its own shell call. Read the verdict before the next call.
  `REFUSED` and `GATHER EVIDENCE` require another consultation. `ASK USER`
  requires the user's answer. Main-agent picks are advisory. Subagent picks
  are binding when the evidence is sufficient.
- Each gate makes at most one Jev request. The HTTP exchange has a three-second
  deadline. Service failures and unreadable transcripts allow the action with
  a visible warning. The inactive command stub reports `decision unverified`.
- Off switch: `AGENT_DECISIONS_OFF=1` in the agent's environment
- Ledger: `~/.local/state/agent-decisions/sessions/<session>.jsonl`
  (override with `AGENT_DECISIONS_STATE`); thresholds in `agent_decisions/policy.py`
- The ledger guard denies any agent tool call that names the state directory,
  including edits to lines of this repository that contain its path. Set the
  off switch to work on those lines.

The hooks send redacted transcript evidence to TypeSafe. A ledger stores the
request and result locally for 14 days. These hooks are a guardrail, not a
security boundary against a process with unrestricted access to the account.

## Tests

- Unit: `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -t . -v`
  (no network)
- Live decision and edit cases: `python3 -B tests/live_cheat.py` (real API, needs the key)
