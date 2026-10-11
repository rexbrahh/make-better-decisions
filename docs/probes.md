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

## v1.0.1 hardening checks (2026-10-11)

The hardening suite uses synthetic files and host records. It does not change
production repositories. The live API checks send synthetic decision context.

| Check | Observed result | Scope |
| --- | --- | --- |
| Offline suite plus macOS isolation | 217 tests passed | User history, quotes, source races, signatures, receipts, cache scope, retries, MCP writes, large inputs, stdin programs, and installer failures |
| Live workflow replay | 32 labelled workflow cases, with results in the local verification artifact | Journal updates, source choices, mechanical tests, large edits, plans, binding choices, user overrides, and questions |
| Live adversarial replay | 16 of 16 labelled cases passed in the final run | Missing options, loaded descriptions, fabricated facts, unconsulted writes, and unrelated verdicts |
| Claude auth status | Signed out | A fresh Claude model-driven session cannot run until the user signs in |

One earlier API run returned a malformed reply. The plugin visibly failed
open, as approved. That result was counted as a failed sample. The subsequent
full run passed. These samples do not establish a general catch rate.

The expanded tests exposed and corrected legitimate-work stalls and bypasses:

- Requested journal updates no longer require an approach choice.
- The complete large tool input is checked as field paths and values.
- Matching decisions remain usable across later turns. Later exact user
  instructions take precedence over binding picks.
- Stop detects an additional choice beside covered choices. A prior question
  tool, Stop retry, or active Stop flag does not exempt a new question or choice.
- Plan checks use the pending plan text. Cached gates require the same host.

The macOS isolation tests require a process outside an existing restrictive
sandbox. Inside the managed outer sandbox, macOS rejects nested sandbox_apply
with EPERM. The final suite ran through the approved escalated test command.
Linux bubblewrap behavior, manual Codex desktop execution, and live Codex
subagent identity have not been verified.

The v1.0.1 activation was interrupted by deletion of a cached entrypoint.
Its global verification did not complete. The v1.0.3 record supersedes it.

## v1.0.2 repair checks (2026-10-11)

The repair suite uses temporary files and synthetic context. It does not change production repositories.

The live replay passed 32 workflow cases, 16 adversarial cases, and 33 supplemental cases. These are sampled results, not a general catch rate.

Earlier repair samples exposed three cases. Missing platform context incorrectly requested a new option list. An exact journal instruction returned an advisory choice. An omitted retry helper escaped the aggregate option check. Separate questions and code composition corrected these cases.

The new checks cover missing entrypoints, missing imports, missing Python, partial startup output, healthy denials, automatic hook feedback, recent diagnostics, aged recipes, and Python startup injection. They also cover active version namespaces and replacement of a managed profile symlink.

The global result and native runtime evidence are recorded in `probe/out/v1.0.2-verification.json` after activation. Claude model acceptance requires an authenticated Claude session.

## Native cache repair (v1.0.3)

The native Codex cache contains Git metadata. The installer must recognize the installed namespace before applying its development-checkout exemption. A regression covers this case.

The v1.0.2 native CLI session verified consultation, the full verdict receipt, covered editing, and Stop through the user's normal wrapper. The configured default profile denied private directory access and installed-cache file creation. No credential contents were read.

The initial native test failures came from test-only CLI overrides. Codex 0.162.1 treats quoted dotted-key segments literally. A partial override for a runtime-only HTTP server also lacks its required transport. Corrected fixture overrides passed.

Inactive-hook reports from older chats are fallback evidence. They do not establish active Jev enforcement. Restart the hosts after an update. Final package identity and activation evidence are recorded in `probe/out/v1.0.3-verification.json`.
