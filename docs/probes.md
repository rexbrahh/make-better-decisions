# Probe results

Observed 2026-10-10 on macOS, Claude Code headless (`claude -p --plugin-dir probe/claude`),
Codex CLI 0.162.1, TypeSafe API. Evidence files are under `probe/out/` (git-ignored; local only).

| ID | Observed | Evidence | Holds |
| --- | --- | --- | --- |
| A1 | `PreToolUse.tool_use_id` equals the transcript `tool_use.id`. At hook time the current call is not yet in the transcript; every earlier sequential call is. A parallel sibling in the same assistant message is not. | `run1/*pre_tool_use.json`, `run2/*pre_tool_use.json` (`ids_in_transcript_at_hook`) | yes, with the limit below |
| A2 | Subagent `PreToolUse` has `agent_id`, `agent_type`; its `transcript_path` is the main session file. `SubagentStop` has `agent_transcript_path` (`<session>/subagents/agent-<id>.jsonl`), `last_assistant_message`, `stop_hook_active`. | `run1/*subagent_stop.json`, `run1/*pre_tool_use.json` (`echo probe-sub`) | yes |
| A3 | Subagent transcript first user line: `isSidechain: true`, `agentId` set, `content` is a string (the parent's brief), no `origin`. | `agent-ac3a974c98c5a84d3.jsonl` | yes |
| A4 | `updatedInput.command` ran (`rewritten`); the transcript keeps the original `echo probe-rewrite`. The deny reason reached the model as `PreToolUse:Bash hook error: probe deny reason 7f3a`; the denied `tool_use` is in the transcript. | `run1/reply.txt`, main transcript | yes |
| A5 | Not probed. | — | pending Rex |
| A6 | Not probed. | — | pending Rex |
| A7 | `jev-1.13.0` accepted; 23,756 input tokens (80,000 chars) answered in 0.52 s, 0.38 s, 0.35 s. | console output of `probe/jev_latency.py` | yes |
| A8 | `ad-probe-bin is on PATH` printed. | `run1/reply.txt` | yes |
| A9 | `~/.codex/config.toml` holds `hooks.state."<plugin>@claude-plugins-official:hooks/hooks.json:…"` trust entries for Claude plugins. | `~/.codex/config.toml` | yes (config evidence; no live call) |

## Consequences

- A1 limit: a ts-decide call and a call that depends on it must not share one
  assistant message. The dependent call's hook cannot see the decide call yet,
  so `gates.consulted` will not count it. No code change: the agent cannot use
  a verdict it has not read, and the skill text says to run ts-decide alone.
- A4: `consulted` matches either `ts-decide` or the verdict file name in the
  call input. The transcript keeps the pre-rewrite command, so the `ts-decide`
  match is the one that fires on Claude.
- A5, A6, desktop hooks: the auto-mode classifier refused both a temporary edit
  of `~/.codex/hooks.json` and `codex exec --dangerously-bypass-hook-trust`.
  Rex runs the Codex probe (plan Task 0 Step 4) before Task 7.
