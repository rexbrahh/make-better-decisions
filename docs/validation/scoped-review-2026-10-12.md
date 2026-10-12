# Scoped review: rationale and evidence

Date: 2026-10-12. Baseline: `4ddc1bd3271181f89b8a5c534ad244af19e9a5bb`. This report records the implementation checks for v1.1.0.

## Recommendation

Keep independent Jev review mandatory for material choices. Enforce evidence provenance, receipt scope, and failures in code. Permit verified parents to accept eligible reviewed options for workers. This keeps routine implementation moving without asking Rex to arbitrate equivalent engineering layouts.

Jev supplies judgments. Source inspection, executable checks, and independent review establish engineering evidence. A confident verdict does not prove code correctness. [TypeSafe describes atomic questions composed in code](https://docs.typesafe.ai/introduction#atomic-questions-composed-in-code) and [confidence derived from the answer distribution](https://docs.typesafe.ai/confidence).

The prevention goal includes omitted consultation, fabricated context, and verdict circumvention. Prompt instructions alone cannot enforce that goal. Protected host records, signed receipts, exact target checks, and owner-controlled failure policy provide concrete checks. Semantic judgments can still be wrong.

## Peer observations

The user supplied two evaluations. They report useful choices around cancellation consistency, retry terminal failures, file ownership, Flush-based cleanup, shared helpers, SDK isolation, and charging applied edits despite presentation errors. Code review and tests found the defects. Jev helped choose responses to those findings.

The reported friction includes four manifest consultations, a 0.43/0.42 preference, blocked worker inheritance, wording refusals, requests for unnecessary user preferences, and trivial administrative consultations. These reports do not measure general model accuracy or total overhead.

The web peer supplied fresh first-hand feedback from its installed 1.0.5 session. Grouped storage-root fields aligned with the C API and binding tests. Its initial request still demanded a distinguishing layout preference. Neutral rewording repaired an argued refusal. It reused prior decisions for routine test corrections. The peer reported 11.8 seconds across four `functions.exec` consultations. That duration does not isolate inference time.

The SDK peer received the feedback request, but no dedicated fresh plugin assessment returned before implementation. Its supplied evaluation and observed native transcript remain the SDK evidence. Final snapshots show model-capacity errors in both peer chats. These errors and native messaging approval review are separate from the decision plugin.

## Implemented changes

| Issue | Change | Evidence |
|---|---|---|
| Parent review failed to cover worker implementation | Hook-owned receipt ids, native ancestry, completed dispatch, repository and path scopes, explicit parent acceptance | `test_receipts.py`: native metadata, changed transport ids, scoped inheritance, foreign actors/repositories, Claude hand-back frames, and checked alternatives |
| Uncertain ranking and generic evidence loops | Per-option invariant checks, worker `PARENT REVIEW`, named blockers, wording fields, stable repair identity, unchanged-result reuse | `test_review_progress.py`: near ties, invariant violations, neutral repair, fresh evidence, no rerolls, and bounded checks |
| Review failure or environment flags bypassed enforcement | Required default, signed installer availability mode, startup deny/block responses, ignored disable flag | `test_required_review.py`, `test_registrations.py`, and installation tests |
| Source revisions and architectural context were unclear | Current/baseline roles, native command metadata, contract hashes, signed durable history, stale source/contract receipt invalidation | `test_continuity.py` and source tests: actual file changes, exit codes, signatures, repository isolation, and fresh-chat denial |
| Context overhead was unmeasured and repeated encoding dominated | Stage and API metrics outside state, model/policy cache invalidation, incremental fit accounting | `test_metrics.py`, evidence budget tests, and fixed-prefix SDK/web benchmarks |

The required-policy regressions failed before production changes. Startup regressions failed for missing Python, entry points, imports, and partial output. Continuity tests exposed missing hashes/history/command metadata. A later regression exposed stale contract receipt coverage. Metrics and wording-repair regressions also failed before their fixes.

During this implementation, installed 1.0.5 again rejected covered changes with a generic reason. Targeted consultations eventually permitted the launcher and serialization patches. The public transcript contained the consultations. The protected runtime eligibility cause was not inspected, so this report does not claim that live incident was diagnosed or fixed.

## Measured context overhead

Three samples per case used fixed transcript byte prefixes and the same source observations. No API calls occurred in these benchmarks.

| Case | Tool calls | Evidence median before | Evidence median after | Retained evidence |
|---|---:|---:|---:|---|
| SDK | 3,175 | 1,040 ms | 716 ms | Identical SHA-256, 79,711 encoded bytes |
| Web | 2,105 | 615 ms | 413 ms | Identical SHA-256, 79,685 encoded bytes |

These final samples show 31% lower SDK and 33% lower web evidence-construction time. An earlier repeat showed about 35% lower time. They do not establish total hook latency, inference speed, or a general workload average. Full transcript decode still costs roughly 0.15–0.23 seconds in the sampled baseline. Protected ledger cost and host scheduling remain separate costs.

The benchmark emits metrics and a hash. It does not expose transcript text. Signed gate metrics record transcript, ledger, evidence, total context, API time, request size, and numeric usage when available.

## Validation and remaining acceptance

Final local validation passed 315 tests in 7.600 seconds on Python 3.9, including macOS isolation. Diff whitespace checks passed.

Real Jev core acceptance passed 33/33 cases with 28 API calls. Ergonomics acceptance passed 32/32 cases with 38 calls. Both returned `jev-1.13.0`, with no API errors. Median API exchange times were 298 ms and 296 ms in these synthetic fixtures. Those timings exclude host hook scheduling and large-chat construction.

A new controlled native-shaped fixture passed five checks with three API calls: scoped invariant review, parent inheritance, implementation approval, actual fixture execution, and routine assertion-correction approval after source changed. This verifies local parsing, signatures, and model interaction. It does not establish live host activation.

The new fixture initially received `GATHER EVIDENCE` because the reader missed a helper filename followed by `?`. A source regression reproduced that omission. The path-boundary fix made the regression and real fixture pass. Jev correctly requested absent evidence in that initial run.

One independent GPT-6 Luna/max reviewer inspected the whole patch and ran 45 focused tests. It found one Important defect: scoped receipts remained eligible after observed source changed without declared contracts. Changed/deleted-source regressions failed, then passed after edit, plan, and final coverage gained fresh snapshot checks. There were no deferred Minor findings and no second review.

The implementation phase preserved installed caches, host settings, and the primary dirty checkout. The user subsequently authorized v1.1 publication, installation, and Codex registration updates. Installation verification is a separate evidence stage.

Captured native fixtures verify parser and receipt behavior. They do not establish new live worker inheritance or GUI activation. Linux bubblewrap runtime behavior and trusted HTTP-service boundaries remain unverified. The optional Codex isolation profile must be selected to apply its filesystem restrictions.

Required review deliberately blocks unchecked work during an outage. Availability provides an explicit operator recovery path and weakens enforcement. Missing Python still blocks reviewed operations because the launcher cannot verify signed availability policy.

Three choices remain conservative: gate caches retain all substantive evidence, scoped receipts require current source snapshots, and nested Claude inheritance remains unsupported. The costs are extra checks after unrelated observations or source edits and refusal to infer unproven ancestry. Routine classification still permits accepted checks and corrections.

The reviewer declined to judge paid replay behavior, live host activation, Linux runtime isolation, and every prompt's semantic quality on arbitrary prose. The real-API runs above address the sampled replay cases. The other acceptance limits remain open.

Future improvements should use recorded workflow outcomes to calibrate thresholds and measure rejected legitimate work. A selective evidence cache or incremental transcript parser needs its own invalidation tests and measurements. These changes are not part of this implementation.
