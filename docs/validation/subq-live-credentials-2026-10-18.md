# Live Snap credential and API qualification

Date: 2026-10-18 UTC.

Both supported credential paths pass against the current Snap API.
The full live suite uses the managed SOPS file. A separate control uses the export from a fresh login shell.
All inputs are existing synthetic configuration fixtures. No historical workflow packet is sent.
No production defect is confirmed. The adapter and installed runtime remain unchanged.

README now states the credential precedence and gives the live-test command.
The managed file is loaded on demand when the environment key is absent.
The opt-in test flag is `AGENT_DECISIONS_SUBQ_LIVE=1`.
These are paid direct-client checks, separate from installed host selection.

## Completed checks

| Check | Result |
| --- | --- |
| Full live suite through the managed file | Four top-level groups, 21 test pass records, zero skips or failures. Package elapsed time is 36.186 seconds. |
| Fresh-login exported key | One 60,000-character code control with 17 questions. Two test pass records, zero skips or failures. |
| Long evidence retrieval | Twelve controls cover prose and code, 60,000 and 780,000 background characters, and records at the start, middle and end. All expected answers pass. |
| Batched and queued operations | The 33-question operation passes in three batches. The 65-question operation passes in five batches and records `api_ms=2248.037667`. |
| Dense inputs | 160,000, 320,000 and 420,000 characters return validated answers. The 480,000 and 1,600,000 cases return sanitized HTTP 400 with no answers or validated returned model. |
| Shared response validator | Five focused race tests pass, with no skips or failures. They cover numeric fields, distribution tolerance, malformed answers, diagnostics and retained metrics. |
| Independent fixture reconstruction | Python independently regenerates all 12 evidence states. Every state hash and encoded length matches the completed Go log. |
| Credential capture scan | The supplied key is absent from all four completed live stdout/stderr files. The record contains no key value or fingerprint. |

The full suite removes `SUBQ_API_KEY` only from its Go child.
The client therefore uses its supported managed-file fallback.
The separate control runs in `zsh -lc`, asserts a nonempty export and uses the ordinary loader.
Native execution records retain both actual commands and successful terminal status.
The managed file belongs to the current user and has mode `0400`.

## Review and evidence corrections

An independent GPT-6 Luna/max peer confirms the live test groups, credential paths, queued operation and dense-input outcomes.
Native turn metadata verifies the model, effort and read-only sandbox.
The reviewer runs no tests or network requests.

The initial preflight incorrectly treats the inherited tool environment as a fresh login.
It exits 1 on its export assertion. Explicit `zsh -lc` and `zsh -lic` checks both find the export.
No configuration change is needed.
Review identifies a missing execution record for this initial failure.
That native record is now retained. Its API-request count was not measured and is not claimed.

The result parser initially misses Go's source-location prefix before log labels.
It is corrected against the same completed logs. No API check is repeated for that correction.
The final parser checks terminal package records, exact test-case sets, model identity and expected outcomes.

## Scope and limits

The adapter SHA-256 remains `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.
The live suite SHA-256 remains `f0efd0f41ce137b8cedab87cdd35b00663f59d7f87c6770288b6a49a460c0a6b`.
The installed runtime remains v2.2.13. No installer or provider-policy command runs.
Only README and this qualification record change.

These results establish current synthetic direct-client integration.
The queued duration is one observation, not a latency guarantee or reliability estimate.
HTTP status alone does not establish the server's precise rejection cause.
Native host behavior, production calibration, workflow benefit and Jev backend repair are outside this check.
No default-provider promotion is selected.

Restricted raw logs, hashes, native invocation records, review and parser outputs remain under ignored `probe/out/subq-followup/live-key-api-f79879b-20261018/`.
