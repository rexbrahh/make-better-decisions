# Probe results

Observed 2026-10-10 on macOS, Claude Code headless (`claude -p --plugin-dir probe/claude`),
Codex CLI 0.162.1 with code mode, Codex app-server, and TypeSafe API.
Evidence files are under `probe/out/` (git-ignored, local only).

| ID | Observed | Evidence | Holds |
| --- | --- | --- | --- |
| A1 | `PreToolUse.tool_use_id` equals the transcript `tool_use.id`. At hook time the current call is not yet in the transcript; every earlier sequential call is. A parallel sibling in the same assistant message is not. | `run1/*pre_tool_use.json`, `run2/*pre_tool_use.json` (`ids_in_transcript_at_hook`) | yes, with the limit below |
| A2 | Subagent `PreToolUse` has `agent_id`, `agent_type`; its `transcript_path` is the main session file. `SubagentStop` has `agent_transcript_path` (`<session>/subagents/agent-<id>.jsonl`), `last_assistant_message`, `stop_hook_active`. | `run1/*subagent_stop.json`, `run1/*pre_tool_use.json` (`echo probe-sub`) | yes |
| A3 | Subagent transcript first user line: `isSidechain: true`, `agentId` set, `content` is a string (the parent's brief), no `origin`. | `agent-ac3a974c98c5a84d3.jsonl` | yes |
| A4 | `updatedInput.command` ran (`rewritten`); the transcript keeps the original `echo probe-rewrite`. The deny reason reached the model as `PreToolUse:Bash hook error: probe deny reason 7f3a`; the denied `tool_use` is in the transcript. | `run1/reply.txt`, main transcript | yes |
| A5 | Codex `PreToolUse` uses `tool_name: "Bash"`, a string `tool_input.command`, `tool_use_id`, and `transcript_path`. The call ID matches the completed `CommandExecution.id`. Earlier sequential commands are present at hook time. | `*pre_tool_use.json`, `codex-cli/reply.jsonl`, the referenced Codex rollout | yes, CLI code mode |
| A6 | Codex executes `updatedInput.command` (`echo rewritten`). The completed execution records this rewritten command. The deny reason reaches the model. A denied command produces no completed execution event. | `codex-cli/reply.jsonl`, the referenced Codex rollout | yes, CLI code mode |
| A7 | `jev-1.13.0` accepted; 23,756 input tokens (80,000 chars) answered in 0.52 s, 0.38 s, 0.35 s. | console output of `probe/jev_latency.py` | yes |
| A8 | `ad-probe-bin is on PATH` printed. | `run1/reply.txt` | yes |
| A9 | `~/.codex/config.toml` holds `hooks.state."<plugin>@claude-plugins-official:hooks/hooks.json:…"` trust entries for Claude plugins. | `~/.codex/config.toml` | yes (config evidence; no live call) |
| A10 | The native manifest selects `codex/hooks.json`. App-server `hooks/list` discovers exactly four plugin hooks, all with expanded package paths and `--host codex`. It does not discover this package's Claude hook registrations. | `/private/tmp/agent-decisions-hook-list.json` | yes, native discovery |
| A11 | All 16 labelled decision and edit cases matched their expected results. Request latency was 0.21–0.65 seconds in the final sample run. | `jev-v1.txt` | yes, this sample only |
| A12 | Temporary `-c hooks.state...trusted_hash` overrides did not establish hook trust. App-server still reported `untrusted`, and the CLI executed the inactive stub. Activation requires persisted trust for the reviewed definitions. | `codex-v1/reply.jsonl`, app-server `hooks/list` with the same overrides | yes, CLI 0.162.1 |
| A13 | With persisted trust, the installed native v1 plugin returned a real Jev `ADVISORY` verdict for UTF-8. The next edit's ledger state contained that verified decision, with `covered` 0.93. `apply_patch` created the expected file, and Stop allowed completion. | `codex-v1-trusted/reply.jsonl`, `/private/tmp/agent-decisions-v1-trusted-smoke/state/sessions/` | yes, native CLI with real API |

## Consequences

- A1 limit: a ts-decide call and a call that depends on it must not share one
  assistant message. The dependent call's hook cannot see the decide call yet,
  so `gates.consulted` will not count it. No code change: the agent cannot use
  a verdict it has not read, and the skill text says to run ts-decide alone.
- A4: `consulted` matches either `ts-decide` or the verdict file name in the
  call input. The transcript keeps the pre-rewrite command, so the `ts-decide`
  match is the one that fires on Claude.
- A5, A6: the Codex parser reads completed execution events and ignores outer
  JavaScript `exec` requests. Agent-written summaries do not become evidence.
  The verdict rewrite preserves a completed execution that the ledger can match.
- A10: native installation replaces the earlier manual hook and AGENTS.md
  pointer instructions. Codex must trust the four current hook definitions.
- A13: the installed plugin completes consultation, verdict read, edit coverage,
  and Stop checks in a native code-mode session. The test used a temporary
  state directory and created only its requested UTF-8 result file.

## Limits

The Codex runtime probe used the CLI. The app-server probe checked discovery
through the desktop protocol. No manual desktop GUI execution was performed.
Codex subagent identity and transcript paths still need a live subagent probe.
The 16 API cases do not establish a general catch rate or decision quality.
