# Native Codex receipt assignment

Date: 2026-10-16. Release: v2.2.7. Base: `e511920`.

## Confirmed failure and repair

Native Codex encrypts the worker brief in its transcript and dispatch hooks.
The transcript still exposes `task_name`, native ancestry and the completed child path.
The previous inheritance function returned immediately when the child brief was absent.
A real worker finished its encoding edit without an inherited receipt. That run did not qualify receipt inheritance.

| Issue | Repair | Evidence |
| --- | --- | --- |
| An encrypted worker brief prevents receipt assignment. | Reserve `ad_` plus the completed receipt ID as the native task name. | The unmodified parser retains the actual 42-character task name and completed child path. |
| A marker alone cannot establish review coverage. | Require the existing parent, session, repository, completed dispatch and current scoped receipt checks. | Positive and negative cases in `internal/hook/receipts_test.go`. |
| An opaque assignment cannot prove an advisory override. | Assign only the receipt's picked option. | The override control retains the original pick and no accepted evidence references. |

The fallback applies only to Codex workers without a readable brief.
Unmarked and malformed tasks retain the previous early return.
Readable Codex briefs and Claude dispatches retain their existing assignment rules.
Provider, model, profile, review contract, scope, source freshness and coverage thresholds retain their existing checks.
Jev remains the normal default with required review. Snap remains opt-in.

## Regression evidence

The new positive regression first failed with zero eligible receipts.
After the repair, it records the same receipt with `parent_dispatch` origin, a binding action and the picked option.
All 38 selected receipt, identity, dispatch, scope, freshness and compatibility cases pass without skips.
They include incomplete dispatch, wrong shared session, wrong parent, wrong repository, stale source and coverage below 0.80.

The final Mac product suite passes 1,051 tests across 23 packages with the frozen Python oracle enabled.
The full race suite also passes 1,051 tests with that oracle enabled.
The 30 skipped cases require other platforms or explicit opt-in modes.
The frozen source is `python-final` at `b5735e6ad1e427167bac37e0a247b8c13483ca03`, with CPython 3.15.0.

The Linux amd64 unit suite passes 677 tests across 23 packages.
It uses the existing local QA image, read-only source, disabled networking, two CPUs and 2 GB of memory.
Its 107 skips include platform-specific and opt-in checks. This Linux run does not enable the Python oracle.

The first broad command also attempted to compile ignored diagnostic helpers with multiple `main` functions.
The complete product checks therefore name `./cmd/... ./internal/...` explicitly.
No diagnostic helper is part of the released engine.

## Verification-harness corrections

A standalone CLI oracle case crossed a one-second boundary between its Python and Go subprocesses.
Only their valid wall-clock timestamps and resulting seals differed.
The comparison now normalizes valid UTC timestamps only for these independent CLI subprocesses.
It verifies each engine's original ledger seals before comparison.
Fixed-clock replays still compare timestamps. Changed content and malformed timestamp controls still differ.
All 22 clock and CLI oracle cases pass.

The native fixture helper now creates directories for its project role file.
Two subsequent native runs changed the captured greeting source while the child was active and recorded no eligible decision.
The final fixture explicitly defers that parent edit until the child completes.
The retained native trace verifies that ordering. No product freshness check or threshold changes.

## Native qualification

The real native worker test passes in 139.885 seconds.
It verifies one GPT-6 Luna/max child, an absent readable brief and the actual 42-character receipt marker.
The worker edit records the parent's receipt, `parent_dispatch` origin, a binding action and the UTF-8 pick.
Its coverage is 0.975 against the unchanged 0.80 worker threshold.
The declared patch updates only `codec.py`. UTF-8 round trips preserve both Zoë and 李.
The parent performs its separate greeting edit after child completion.
Neither native thread reads memory. Captured process output contains no supplied Snap key.

The Codex parent-host case passes in 42.15 seconds with receipt coverage 0.978 against 0.50.
The authenticated Claude primary-root case passes in 19.88 seconds with receipt coverage 0.977 against 0.50.
Both cases verify the encoding and the separate exact greeting edit on the same test source.
All three native cases reject invalid-hook JSON and the shared-server profile warning.

An independent GPT-6 Luna/max source review finds no concrete bug in the production patch.
The root verifies the reader's actual native model and effort metadata.
The reader does not run tests.
A second Luna/max reader finds one test weakness: the native helper checks for a parent ID without comparing it to the dispatching parent.
The helper now loads both native transcripts and compares parent ID, shared session and the matching completed dispatch.
An offline check of the same retained real threads passes. A wrong-parent control fails as expected.
These checks make no provider calls. The root verifies the second reader's actual model and effort.
Focused race checks, vet and whitespace checks pass after the final test corrections.

One documentation edit reports `jev unreachable` and does not execute.
The existing live encoding control then returns ADVISORY in 0.17 seconds, and the exact edit succeeds on retry.
This verifies recovery for that operation. It does not establish the backend cause.

Private diagnostic records remain under `probe/out/subq-followup/native-inheritance/`.
No modified historical workflow packet is replayed. No SDK source, builder or shared cache is used.

## Release and installation

Both candidate release binaries build and match their pins.

| Asset | SHA-256 |
| --- | --- |
| Darwin arm64 | `1b593fb721649801f29b47c3b01f4bbd841112cbc5a1aa1b2aeab46056b44932` |
| Linux amd64 | `612be1874a0306a3f50e396eebbbbb91ba7892aa298cc20b1f9099bcf603d831` |

Signed source commit: `d5de3035cdc37b56f31b6472eabf942d93eca3e5`.
The signed `v2.2.7` tag verifies. Both downloaded release assets match the listed pins.
Release: https://github.com/rexbrahh/make-better-decisions/releases/tag/v2.2.7

Codex and Claude plugin caches report v2.2.7 and match all seven checked public registration and runtime-source files.
Claude user and project registrations identify the signed release commit.
The authenticated primary configuration shares those plugin registrations through its existing plugins-directory symlink.
The protected final installers succeed for both hosts using the downloaded Darwin binary.
Their plans retain Jev with required review.

The active runtime is `~/.local/share/agent-decisions/runtimes/83524658238943e68495d0973b4b2d55`.
It reports 2.2.7 and matches the Darwin pin.
All ten runtime files match the candidate used for the real Snap worker and parent-host checks.
All three owned Codex hooks are enabled and trusted in repository and home scopes.
Fresh Codex and authenticated Claude sessions capture exact factual reads with active hooks and no unsolicited consultation.
Those sessions report no invalid-hook JSON or shared-server profile warning.

Codex default policies, both hosts' MCP registrations and the primary Claude settings symlink remain unchanged.
Existing SDK sessions are not restarted or individually requalified.
Peer-owned `evals/`, `docs/evaluation/` and `docs/handoff.md` remain outside the release commits.
