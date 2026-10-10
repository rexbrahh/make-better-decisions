# agent-decisions

Hooks that make Claude Code and Codex consult TypeSafe's Jev model before an
agent picks between approaches or asks the user to decide. Design: `docs/spec.md`.

- Tests: `PYTHONDONTWRITEBYTECODE=1 python3 -m unittest discover -s tests -t . -v`
- Live cheat cases: `python3 -B tests/live_cheat.py` (real API, needs the key)
- Off switch: `AGENT_DECISIONS_OFF=1` in the agent's environment
- Ledger: `~/.local/state/agent-decisions/sessions/<session>.jsonl`; thresholds in `agent_decisions/policy.py`
- The ledger guard denies any agent tool call that names the state directory,
  including edits to lines of this repository that contain its path. Set the
  off switch to work on those lines.
