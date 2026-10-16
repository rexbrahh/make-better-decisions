# SubQ Snap review provider

Status: owner selected the separate client on 2026-10-15. The client and integration are implemented.
The [QA record](../../validation/v2.2.0-qa-2026-10-16.md) tracks qualification and release evidence. Jev remains the default.

## Goal

Add an owner-selectable SubQ Snap backend. Keep Jev as the installed default during qualification.
The same decision, edit and plan gates must enforce the same authority rules with either backend.
Provider availability must not weaken a healthy denial or grant permission for an unresolved choice.

## Observed constraints

| Constraint | Required change | Evidence |
| --- | --- | --- |
| Every ordinary two-option consultation creates at least 17 model questions | Split Snap questions into batches of at most 16 | `internal/gates/gates.go:453`; the live 17-question Snap request returned HTTP 422. |
| The current API deadline is three seconds | Give Snap one bounded deadline across all its batches | `internal/jev/jev.go:305`; four concurrent long-input probes took 1.71 to 3.76 seconds each. |
| Model identity is absent from reusable receipt records | Bind new-provider receipts, caches and inheritance to an explicit review identity | `gates.Consulted`, `reuseReview`, `cacheContract` and `hook.inheritedDecisions`. |
| Evidence and source excerpts have fixed Jev limits | Add a provider-specific evidence profile without changing Jev defaults | `evidence.Build`, `TrimToBudget`, `readSource`; middle-record probes passed at 200,048 and 800,048 characters. |

## Owner selection and compatibility

Add `agent-decisions-install --review-provider jev|subq` to the protected installer.
Store the selected name in the signed host installation specification.
An omitted flag preserves an existing valid selection. A legacy specification selects Jev.
An unknown provider fails closed. Tool input and agent environment variables cannot select a provider.
Do not change the Mac's default provider during this implementation.

Load the selection once for a reviewed hook operation and pass it through the runtime context.
Keep ordinary tools independent of the remote review service.
Preserve the complete frozen Python oracle for the default Jev path.

Snap uses model `subq-snap-preview` and the fixed endpoint `https://api.subq.ai/v1/systemone`.
Read its key from `SUBQ_API_KEY`, or the standard SOPS-Nix `subq_api_key` secret path.
Scrub both available provider keys before evidence, questions and ledger records are serialized.
Do not store keys in installation policy, command arguments, release artifacts or test logs.

## Selected client boundary

Add `internal/subq/` for the Snap client.
Extract the existing response validation from `jev.Ask` into `jev.ValidateResponse` and reuse it.
Keep the Jev transport, timeout and context-limit retry behavior unchanged.
The current gates do not use score questions; score support is outside this integration.
The following transport contract applies to the Snap client.

Snap batches contain at most 16 questions in deterministic order and see the same scrubbed state.
Use at most four simultaneous API requests within one reviewed operation.
All batches share a ten-second deadline and cancellation. Partial answers cannot reach a gate.
Validate every requested answer, the response model, and each choice distribution before merging results.
Cap request bodies at 2 MiB and successful response bodies at 1 MiB.
Reject redirects. Report sanitized status or transport errors without response bodies.
Do not retry with a different provider or silently shorten authority inputs.
An oversized or failed Snap request fails closed under required review.
Keep host hook timeouts unchanged; measure the complete reviewed operation against that budget.

Record provider, requested and returned model, batch count, request sizes, timing and token usage.
Metrics stay outside model state. Report total usage across batches without double counting.

## Evidence and review identity

Use an initial Snap evidence budget of 800,000 serialized characters and 160,000 characters per text excerpt.
Keep the existing file-size, candidate-count, included-file-count and request-input limits.
Add a profile to `evidence.Options` and pass it through source excerpts, parent evidence,
contract observations and final trimming. Provider-profile regression tests cover this parameter flow.
Retain redaction before clipping, full-file hashes, trust labels and omission counts.
This character budget does not guarantee a token count; the API can still reject a request.
Do not claim the full 384K window has been qualified from the initial 139K-token probe.

Snap review identity includes provider, model, context-profile revision and review-contract revision.
Load it with `ledger.ReviewProvider` and carry it in `gates.Context`.
Include it in scoped fingerprints and automatic gate cache contracts.
Persist it on decision records before receipt creation.
Check it on completed local decisions and inherited parent decisions before they become eligible.
Treat legacy records without that field as Jev records only.
Changing the owner-selected provider requires fresh review for material work.

## Verification and release

First add failing tests for provider selection and preservation, batching, cancellation, malformed answers,
bounded bodies, secret redaction, larger evidence, and receipt/cache/inheritance separation.
Then implement the selected client boundary and run the smallest relevant checks.
Run the unchanged Jev oracle, local race checks, live Snap gate controls and both native host fixtures.
Use temporary signed installation state for Snap host tests; retain the normal Mac default.
Obtain independent source review of the completed diff before release.

Compare providers on fixed, labeled cases. Agreement with Jev is not ground truth.
Include healthy denials, exact instructions, missing prerequisites, malformed replies, source provenance,
long-context retrieval, outages and a repeated-review sequence.
Record reliability, false releases, false blocks, latency and token cost separately.
An initial opt-in release does not establish readiness to replace Jev.

## Primary references

- [SubQ Decisions API](https://docs.subq.ai/endpoints/decisions-api/)
- [SubQ models](https://docs.subq.ai/using-the-api/models/)
- [SubQ authentication](https://docs.subq.ai/using-the-api/authentication/)

Raw source audits and initial API measurements are under ignored `probe/out/subq-campaign/`.
