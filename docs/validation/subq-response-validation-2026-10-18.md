# Snap response validation coverage

Date: 2026-10-18. Baseline: `367ed48102eed4cd3918481478e362e46ad1772b`.

The current adapter enforces the selected response contracts. This change adds regression coverage for two confirmed test gaps.
An independent source reviewer identified missing model-type cases and unobserved closure of oversized response bodies.
The new tests reuse the existing fake transport, response helpers and observed body.

| Contract | Added scenarios | Assertions |
| --- | --- | --- |
| Bound response consumption and close bodies. | Valid JSON at 1 MiB, one byte over, 1,024 bytes over, and complete valid JSON followed by injected `io.ErrUnexpectedEOF` or a synthetic reader error. | Accept the exact limit. Reject failures without answers. Consume at most 1 MiB plus one byte from the body. Close each body and retain a sanitized diagnostic. |
| Validate the returned model. | Missing, null, boolean, numeric, array and object model fields. | Return `malformed: response model`, no answers and no validated returned-model metric. Close each body. |

After each body scenario, the same client completes a 64-question recovery call.
Four synchronized requests must start before any can complete. This checks that all four shared slots remain available.
The read measurement applies to `response.Body`. It does not measure transport buffering or network bytes.

## Fault controls

Each temporary source overlay passed the previous offline suite: 40 named passes and four live-test skips.
The new controls reject each fault at the expected assertion.

| Injected fault | Failing new child cases |
| --- | ---: |
| Leave bodies open after size or read errors. | 4 |
| Read the body without the byte limit. | 1 |
| Accept a missing or non-string model. | 6 |

These faults exist only in disposable overlay files. Production source remains unchanged.

## Final checks

The affected package passes on macOS with race detection: 53 named passes and four opt-in live-test skips.
It also passes on Linux amd64: 53 named passes and the same four skips. The Linux run does not use race detection.
Both runs use Go 1.27.2. Vet and formatting checks pass.
Linux uses an existing pinned local image, no network, a non-root user, read-only source and toolchain mounts, and temporary build storage.

Retained evidence is in `probe/out/subq-followup/response-body-boundary-20261018/`.
`fault-verification.json` records the fault controls. `verification.json` records both final package runs and source hashes.
The independent discovery review is in `probe/out/subq-followup/client-boundary-audit-20261018/`.

No live-provider or private-transcript evaluation ran for this change.
This coverage does not establish production calibration or provider recovery.
The change contains only tests and documentation. It requires no binary release or installation.
