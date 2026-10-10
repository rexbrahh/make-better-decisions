# make-better-decisions

Hooks that make Claude Code (and, once verified, Codex) consult TypeSafe's Jev
model before an agent picks between approaches or asks the user to decide.
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

## Codex

`codex/` holds the Codex hook registration and an `AGENTS.md` pointer. The Codex
payload shapes are not probed yet (`docs/probes.md`, rows A5 and A6), so treat
Codex support as unverified. `probe/` holds the probe hooks used to fill those rows.

## Operation

- Off switch: `AGENT_DECISIONS_OFF=1` in the agent's environment
- Ledger: `~/.local/state/agent-decisions/sessions/<session>.jsonl`
  (override with `AGENT_DECISIONS_STATE`); thresholds in `agent_decisions/policy.py`
- The ledger guard denies any agent tool call that names the state directory,
  including edits to lines of this repository that contain its path. Set the
  off switch to work on those lines.

## Tests

- Unit: `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -t . -v`
  (no network)
- Live cheat cases: `python3 -B tests/live_cheat.py` (real API, needs the key)
