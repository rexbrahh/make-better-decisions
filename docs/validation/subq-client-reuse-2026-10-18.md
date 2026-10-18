# Snap client reuse after a failed batch

Date: 2026-10-18 UTC. Source baseline: `5f5ebfd786ff5194eaac0d9ad8b1666ff8b8ac18`.

The existing client uses per-call cancellation and shared request slots.
The earlier [malformed-batch check](subq-batch-failure-2026-10-17.md) verifies cancellation and rejects partial answers.
This extension checks that the same client can complete another call after that failure.
No production defect is confirmed. Production code remains unchanged.

| Coverage gap | Added check | Evidence |
| --- | --- | --- |
| A failed call could leave occupied slots without failing the earlier test. | Reuse the same client for four simultaneous recovery batches. | The old test passes with an injected slot leak. The extended test fails at its recovery assertion. |

## Fixture

The first call retains the completed, malformed and blocked batches from the earlier fixture.
It must return the original malformed error, no partial answers and no validated returned model.
The blocked sibling must stop through cancellation. Both response bodies must close.

The second call submits 64 questions through the same client.
Each of its four transport callbacks waits until all four callbacks arrive.
An occupied slot from the failed call prevents that barrier from completing.
The second call must return every answer and record its validated model.
The existing one-second fixture deadline bounds a failed recovery.
The fixture uses the existing simulated transport and a dummy key.

## Verification

| Check | Result |
| --- | --- |
| Twenty Mac race repetitions of the extended test | 20 pass, no skips |
| Complete Mac product race suite with the frozen CPython reference | 1,213 test pass records across 23 tested packages, 20 test skips and three helper packages without tests. All 26 packages terminate without failure. |
| Linux amd64, non-root, offline container | Ten repetitions pass, no skips |
| Leak request slots only when a batch fails, using a disposable Go overlay | The baseline test passes. The extended test fails because the shared client cannot recover all slots. Neither working source file changes. |
| Independent GPT-6 Luna/max source review | No actionable finding. Native turn metadata verifies the model and effort. The reviewer runs no tests. |

Vet, formatting and whitespace checks pass.
The result parser rejects an incomplete suite log.
Both evidence/Python comparison tests pass explicitly.
The Linux container uses read-only source, the existing local toolchain and no external network.

The first focused launch cannot write its default Go cache inside the sandbox.
The corrected launch uses a writable temporary cache and passes.
The first complete Mac run passes with eight additional native isolation skips because nested sandbox initialization is unavailable.
The final host-context run passes all eight cases. Original logs remain retained.

Production client SHA-256 remains `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.
The final test file has SHA-256 `3a10add4076f0e18026c699b749ddd515cfde658baefed9a294c984cb89307b3`.
Only test and documentation files change. The installed binary remains v2.2.13.
No installer, provider, policy or SDK state changes occur.

These checks verify local client recovery. They do not establish provider connectivity or production calibration.
Raw results remain under ignored `probe/out/subq-followup/client-reuse-20261018/`.
