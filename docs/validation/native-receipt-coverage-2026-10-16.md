# Native Snap receipt coverage

Date: 2026-10-16. Source baseline: `97ddf96`. Final test implementation: `f9a62cf`. Installed runtime: v2.2.6.

The [native test](../../cmd/agent-decisions/subq_host_test.go) now checks a selected implementation edit against its completed Snap receipt.
The previous test checked a receipt and an unrelated exact edit.
Both native hosts pass the stricter path, scope and receipt assertions on the same final test source.
The successful Claude run explicitly selects the installation's existing authenticated configuration root with `CLAUDE_CONFIG_DIR`.
An earlier authentication failure remains recorded. Checking only the default root did not establish that the selected installation root needed login.

## Required observations

Each host uses a temporary repository and temporary signed Snap policy.
The normal installations retain their provider and review policy.
The fixture contains a contract, an unset encoding assignment and a greeting function.

1. Read the synthetic contract. Submit one scoped encoding consultation, without retries.
2. Check the completed verdict, selected UTF-8 option, provider identity, model, context profile, review contract and two consultation batches.
3. Find separate allowed edit requests for the exact fixture files. Require the encoding receipt in its recorded decisions and coverage at the applicable policy threshold.
4. Parse the final encoding assignment without executing the generated source. Check UTF-8 and both name round trips.
5. Check the exact greeting change and repository status. Only the two intended tracked files may change, with no new unignored files.

The consultation scope must contain `codec.py` and the required name-preservation invariant.
Root edits use `policy.FollowAt`. Worker records use `policy.BindAt`, consistent with the edit gate.
These runs exercise native parent sessions. They do not exercise worker inheritance.

## Repairs to the test

| Confirmed issue | Correction | Evidence |
| --- | --- | --- |
| Current Codex hook input records the patch under `command`. The first matcher expected `input`. | Accept either recorded shape. Reject both together. | Original native patch calls and recorded input key names |
| Codex persisted trust for temporary repositories. A quoted dotted override produced a project key containing literal quotes. | Use a canonical path inside an inline TOML `projects` table for each invocation. | Local app-server parser probe and native before/after configuration hashes |
| Basename matching accepted an unrelated `codec.py`. | Resolve relative paths from the fixture and compare canonical file paths. Check the recorded consultation scope. | Two negative controls fail before repair. All 18 final path controls pass afterward. |
| One patch could count as both separate edits. | Accept patches with exactly one updated file and no added or deleted files. | Three additional negative controls fail before repair and pass afterward. |
| Coverage failures hid receipt mismatches. Process failures discarded stdout. | Log the receipt-match boolean, coverage score and threshold. Include sanitized stdout in process errors. | Independent review and Claude's original native authentication error record |

The first two native attempts appended one temporary trust entry each to Codex configuration.
Each exact inverse reproduced the original configuration hash before restoration.
The retained restoration records confirm that only the owned append was removed.
The final inline override needs no configuration writer.
A temporary cleanup-writer prototype was removed and is not part of this change.

## Verification

| Check | Observed result |
| --- | --- |
| Final Mac command tests | 34 passed, including 18 path controls. Three paid native suites remain opt-in. |
| Final Linux amd64 command tests | 32 passed, including all 18 path controls. Three native suites and two existing Python-dependent tests skipped. |
| Native Codex factual read with the inline override | PASS in 16.52 seconds. Both settings files remain unchanged. No consultation was requested. |
| Final native Snap run on Codex | PASS in 46.22 seconds. Receipt matched, coverage 0.982, threshold 0.50. Both settings files remain unchanged. |
| Final native Snap run on Claude | PASS in 19.25 seconds. Receipt matched, coverage 0.976, threshold 0.50. Both settings files and the symlink remain unchanged. |

`go vet ./cmd/agent-decisions` passes on the final source.
Linux uses the existing local QA image, a read-only Go toolchain, read-only source, no network, two CPUs and 2 GB memory.
It does not use an SDK builder or shared cache.

Both final native runs use the same test source, including the process-error diagnostic and separate-edit corrections.
The test SHA-256 is `3b8190f36f519deff1bff9d8156c0dfd133f9661dd4766f148a61115869dddd2`.

The earlier strict Claude attempt failed before model work in 1.36 seconds with `authentication_failed`.
A later supported CLI check distinguishes two existing configuration roots: `~/.claude` is logged out, while `~/.claude-primary` is authenticated.
They are separate directories, although the primary root links its settings and project directory to the default root.
The successful case sets `CLAUDE_CONFIG_DIR` to the selected primary root for the test process only.
The test reuses the primary root's existing authentication. The environment selection applies only to the test process.

Before a paid native check, set `CLAUDE_CONFIG_DIR` to the authenticated root used for installation.
Check `claude auth status` in that same environment.
An authentication failure applies to that root. Shared settings do not establish shared authentication.

Before and after the completed native attempts, the runtime retains Darwin SHA-256:
`637af1cd55219f63ee479e6b65adb2a8900b6367ad7e8b5294afecf68b694963`.
The final native attempts preserve both settings files, their modes and the Claude settings symlink.
No supplied Snap key appears in captured output. No historical workflow packet was exported.

The factual-read runner incorrectly labeled its policy metadata as a temporary Snap selection.
That fixture uses fresh state and requests no review. Its result qualifies routine execution and configuration preservation only.

## Review and limits

The completed independent GPT-6 Luna/max review identified the basename assertion and missing failure diagnostics.
Those findings produced the corrections above.
The first reader attempt timed out without a final report. A later sandboxed launch could not initialize its app-server.
Neither failed launch counts as a completed review.
A second independent GPT-6 Luna/max review confirmed the path, scope and diagnostic corrections.
It identified the combined-patch assertion and the ignored-artifact limit.
The parent corrected the patch matcher and repeated the checks on the final source.
The two earlier readers have verified native model and effort records. Neither reviewed the last patch-matcher correction.
A later independent GPT-6 Luna/max reader inspected that final matcher, all 18 controls and the selected-root Claude records.
It reported no concrete finding within that scope. It inspected retained results rather than running tests.

These checks establish a bounded native receipt-to-edit observation on the passing fixture.
They do not establish model calibration, default-provider readiness, worker inheritance or behavior on arbitrary implementations.
The ledger records an allowed tool request. Final file acceptance provides a separate output check, without replaying the patch parser.
The fixture ignores `__pycache__/`. This test does not inspect that cache or prove the absence of ignored artifacts.
The final native process-error diagnostic is source-reviewed but not exercised by a new native failure.

Only tests and documentation change. No production code, binary release or installation changes in this round.
Jev remains the normal default. Snap remains opt-in.
Raw attempts, parser probes, restorations, reviews and source manifests are under ignored `probe/out/subq-followup/native-covered-edit/`.
Final offline and Codex native evidence is in `final-v2/`. Final Claude native evidence is in `primary-claude/`.
The earlier Claude authentication failure remains in `path-binding/`.
