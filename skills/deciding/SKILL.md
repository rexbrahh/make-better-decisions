---
name: deciding
description: Use before discretionary edits to code or docs, before choosing between approaches, designs, libraries, or root-cause theories, and before asking the user to decide. Runs ts-decide with evidence the hook reads from the session transcript.
---

# Deciding with ts-decide

Run `ts-decide` when you are about to:

- edit code or docs that select an implementation, design, or product behaviour;
- choose between two or more viable approaches, designs, libraries, or root-cause theories; or
- ask the user to make a choice that may be yours to make.

Do not run it to carry out an explicit instruction from the user.
The instruction must specify the change exactly. A general request to fix or
build something does not specify its implementation.

The hook asks Jev to judge every edit and shell command that changes files.
The hook denies an edit when its choice lacks a relevant completed verdict.
Consultation on an unrelated choice does not authorize the edit.

## Call

`ts-decide` must be the whole command, in a tool call of its own: never in parallel with another call, and
never in the same message as the call that acts on its verdict. Use a heredoc so quotes need no escaping:

Use the absolute command path supplied by SessionStart. If that context is
unavailable, locate `bin/ts-decide` two directories above this skill directory.

```sh
ts-decide <<'EOF'
{"decision": "Which eviction policy for the tile cache?",
 "options": [{"id": "lru", "description": "Least recently used"},
             {"id": "lfu", "description": "Least frequently used"}],
 "facts": ["tiles are re-read in bursts"]}
EOF
```

- `options`: every viable option, two or more. Ids use `a-z 0-9 _ -`. Descriptions are neutral, one line. `none_fit` is reserved.
- `facts`: excerpts copied verbatim from the user's messages or from tool output in this session. A fact not found verbatim is shown to Jev as an unverified claim.
- State facts; do not argue for an option. The hook reads the transcript itself.

## Verdicts

| Verdict | Do this |
| --- | --- |
| `BINDING` | Follow the pick. Subagents only. |
| `ADVISORY` | Take the pick, or override it and state the evidence that outweighs it. |
| `GATHER EVIDENCE` | Investigate with tool calls, then call again. |
| `ASK USER` | Ask the user. Give the options, with the pick as your recommendation. |
| `REFUSED` | Add the missing option or restate the facts neutrally, then call again. |
| `decision unverified` | The service failed. Decide yourself and state your evidence. |

After `ASK USER`, wait for the user's answer before editing.
Claude Code also checks `AskUserQuestion` and `ExitPlanMode`.
Codex checks `request_user_input` and `request_user_input_async`.
Both hosts check edits, shell writes, and final replies.
A denial or block states the required next action.
