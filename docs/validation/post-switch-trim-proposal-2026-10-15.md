# Post-switch trim proposal

Date: 2026-10-15. Baseline: `598cd98`, before any trim. Status: CLI trim implemented, caller-frame trim deferred.

The handoff asks for line counts and risks before implementation. The measurements below precede implementation.
See the [QA report](go-qa-2026-10-15.md) for implementation results.

## Scope and measurements

Counts include comments and blank lines. A candidate count is not a net deletion estimate.

| Item | Current code | Proposed change | Risk and required check |
| --- | --- | --- | --- |
| Choice suggestions | `internal/py/argparse/difflib.go`: 129 lines. Its only production caller is the four-line suggestion branch in `checkValue`. | Remove the helper and the suggestion branch. Keep invalid-choice rejection, valid-choice listings and exit status 2. | Low. CLI error text changes. Keep negative argument tests and valid-choice parsing tests. |
| Help wrapping | `internal/py/argparse/textwrap.go`: 114 lines. Its production caller is `splitLines`, used by the help formatter. | Remove the copied wrapping algorithm. Print help descriptions without width-dependent wrapping. | Medium. Long help lines and narrow terminals change. Keep all flags, descriptions and subcommands visible. |
| CLI colours | The theme, colour detection and ANSI removal occupy lines 520–559 of `argparse.go`. References also occur in usage and action formatting. | Remove colour selection and colour markup. Use plain help and CLI diagnostics. | Low. Terminal presentation changes. Keep diagnostic content and exit statuses. |
| Python caller-frame arithmetic | 144 lines mention frame symbols across five production files. See the table below. | Remove Python caller-stack offsets. Retain explicit limits for recursive operations on untrusted input. | High. These counters currently enforce limits. Preserve redaction, private-path checks, source budgets and error handling. |

The two standalone presentation helpers total **243 lines**. The presentation block in `argparse.go` is **344 lines** (521–864).
That block also contains necessary usage and help formatting. It cannot all be deleted.

| Production file | Lines mentioning frame symbols |
| --- | ---: |
| `internal/gates/values.go` | 1 |
| `internal/gates/gates.go` | 65 |
| `internal/evidence/evidence.go` | 40 |
| `internal/hook/hook.go` | 27 |
| `internal/transcript/transcript.go` | 11 |

The frame count uses a case-insensitive search for word symbols containing `frame`.
It measures the review surface. It does not mean 144 lines are safe to delete.

The JSON decoder and encoder need a separate boundary review. They contain 605 and 242 lines respectively, including serialization logic.
Their emulated stack thresholds affect overflow handling. Their canonical JSON logic is outside this trim.

## Callers and checks that must change together

The shared argument parser serves the hook, installer and isolation CLI.
Presentation changes reach `internal/py/argparse/argparse_test.go` and the oracle tests in those three packages.
Replace exact presentation comparisons only where this proposal changes presentation.
Retain argument semantics, required flags, accepted choices, help availability and exit statuses.

Frame changes reach gates, evidence construction, hook context and transcript loading.
Review the JSON boundary before changing its limits.
Keep deep-input, secret-redaction, private-state, brace-expansion, glob and source-budget tests.
Do not remove a guard because it also models Python frames.

## Permanent compatibility checks

1. Preserve canonical ledger JSON and HMAC seals.
2. Preserve gate fingerprints, `input_sha256` and transcript user keys.
3. Preserve hook response JSON and deny text.
4. Preserve native identity, receipt scope and private-state protection.
5. Run unit, oracle, real-transcript replay, race, live Jev and Linux isolation checks after each accepted trim.

## Acceptance boundary

The presentation trim and frame trim require separate implementation diffs because their risks differ.
The owner accepted the proposal, then clarified that recursive improvement describes the development workflow.
The CLI trim is implemented. Caller-frame removal and changes to recursion limits are deferred.
Publication remains a separate owner action.
The missing-path Linux isolation policy remains a separate owner decision.
