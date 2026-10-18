# Snap qualification rejects service failures

Date: 2026-10-18 UTC.

A Snap qualification subtest passed when its review service returned HTTP 401 or 503.
The production gate correctly returned `deny` after the failed review.
The test accepted that action without marking the service error as a failure.
This was a qualification defect, not a runtime permission bypass.

## Repair

The two affected Snap test callbacks now call `t.Errorf` when the client returns an error.
They then return the original answers and error.
This follows the existing pattern in the provider-comparison and long-source authority tests.
It also protects the review sequence's unrelated-edit denial from masking a service failure.

The change touches `internal/gates/subq_live_test.go` and `internal/gates/provider_live_test.go`.
Production source, thresholds, provider identity and review policy remain unchanged.

| Confirmed issue | Correction | Evidence |
| --- | --- | --- |
| An unavailable review can satisfy the expected `deny` action. | Mark the API error as a test failure before returning it. | Offline HTTP 401 and 503 controls pass before the repair and fail afterward. The gate still returns `deny`. |

## Verification

| Check | Result |
| --- | --- |
| Injected HTTP 401, before repair | The denial subtest and parent both pass despite the observed fake service failure. |
| Injected HTTP 503, before repair | The same false pass occurs. |
| Both controls, after repair | Each process exits 1 and names its service error. Both gate outcomes remain `deny`. |
| Complete affected package with race detection | 97 test pass records, 10 skips, no failures |
| Actual Snap basic qualification and review sequence | Eight test pass records, no skips or failures |
| Vet, formatting and whitespace checks | Pass |
| Independent GPT-6 Luna/max source review | No confirmed bugs or material gaps. Native metadata verifies model, effort and read-only permissions. |

The offline controls replace the client's transport through disposable Go overlays.
The fixed fake transport returns the chosen status and never forwards a request.
They use a fixture key and make no external HTTP request.
The first overlay has an unused-variable compiler error. Its logs are retained.
The corrected controls compile and show the false pass before any test source changes.

The live basic cases retain their expected advisory, evidence-gathering, refusal, routine, allow and deny outcomes.
The sequence makes four healthy review invocations.
It verifies exact and rephrased reuse, a covered edit, an unrelated denial, fresh source review and provider isolation.
These are existing synthetic fixtures. No private workflow packet is sent.
The supplied key is absent from both captured live output files.

## Boundaries

The production client SHA-256 remains `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.
The relevant production gate and policy files still match v2.2.13.
No binary release, installer, provider change or SDK action is required.
This repair prevents service outages from qualifying as healthy denials.
It does not identify historical Jev outage causes or establish production calibration.

Raw controls, terminal logs, source hashes, review and result checks remain under ignored `probe/out/subq-followup/gate-failure-qualification-20261018/`.
