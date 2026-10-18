# Shared qualification rejects unavailable reviews

Date: 2026-10-18 UTC.

Four existing Jev cases passed when the service returned HTTP 401 or 503.
The production gates correctly denied or blocked the unchecked operation.
The shared test runner accepted those actions as successful qualification.
Its filtered summary also counted cases that did not run.

## Repair

The shared `expect.met` helper now rejects unavailable-review outcomes before it compares actions.
It recognizes the existing unverified action, required-review messages and availability warnings.
This includes aggregate timeouts returned before background review requests finish.
The runner counts a case only when its subtest executes.

The helper serves the Jev scripts, Snap gate qualification, provider comparisons and the Snap review sequence.
The regression test obtains actual error outcomes from all five gates in required and availability modes.
It verifies that a matching action cannot qualify an unavailable review.

| Confirmed issue | Correction | Evidence |
| --- | --- | --- |
| Service errors satisfy expected deny or block actions. | Reject the gate's unavailable result in the shared expectation. | Fixed HTTP 401 and 503 controls pass before the repair and fail afterward. |
| A delayed request can change a shared failure counter after its originating case passes. | Check the returned outcome within that case. | The first candidate reports `1/1` before its parent fails. The final repair fails the originating child and reports `0/1`. |
| Filtered cases inflate the denominator. | Increment the total inside the executed subtest. | Four selected cases previously report `4/33`. Both repaired failure controls report `0/4`. |

## Verification

| Check | Result |
| --- | --- |
| Ten actual unavailable gate outcomes, before the expectation repair | All ten regression cases fail for the expected false acceptance. |
| Same regression cases, after repair with race detection | All ten pass. |
| Fixed HTTP 401 and 503 controls | Each fails all four originating cases and reports `0/4`. Existing gate actions remain deny or block. |
| Delayed multipart review | The aggregate timeout fails its originating child. The summary reports `0/1`. |
| Complete affected package with race detection | 110 test pass records, ten skips, no failures. |
| Actual Jev and Snap fixtures | Seven Jev cases and two Snap cases pass, with 13 test pass records and no skips or failures. |
| Remaining shared-helper callers | Four provider-comparison cases, the Snap review sequence and six long-source authority cases pass. The run has 18 test pass records, no skips or failures. |
| Vet, formatting and whitespace checks | Pass. Each command's exit status and output hashes are retained. |
| Independent final source review | No confirmed source bugs. Native metadata verifies GPT-6 Luna/max and read-only permissions. |

Offline controls use fixture keys and fixed fake transports that never forward a request.
The delayed transport returns HTTP 503 after the shorter aggregate review deadline.
Its first rebuilt overlay has a missing test import. That compiler failure remains recorded separately.
The corrected overlay compiles and confirms the child failure.
All original attempts and first-candidate evidence remain retained.

The paid checks use existing synthetic fixtures.
They cover expected advisory, routine, allow, deny and refusal outcomes across every caller of the shared expectation.
The sequence verifies receipt reuse, source refresh and provider isolation.
The six long-source cases cover source-only claims and exact user instructions at three source positions.
The supplied keys are absent from all four captured live output files.
No private workflow packet is sent.

The final reviewer identified a missing retained exit status for the vet claim.
The follow-up records successful vet, formatting and whitespace commands in `terminal-checks.json`.
The reviewer also identified skipped live callers. The additional completed run covers them without a source change.
Large JSON log messages require concatenation by test before decoding.
The result parser was corrected against the same terminal log. No paid case was repeated for that correction.

## Boundaries

Only test source and this record change.
Production deadlines, gates, policy, thresholds and provider selection remain unchanged.
No binary release or host reinstallation is required.
These checks do not establish production calibration or explain historical Jev outages.

Raw controls, terminal logs, source hashes and review records remain under ignored `probe/out/subq-followup/jev-gate-failure-qualification-20261018/`.
