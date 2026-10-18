# Malformed choice diagnostics remain unverified

Date: 2026-10-18 UTC.

A peer reports an explicit consultation failure:

```text
ts-decide: decision unverified (jev malformed: missing_fact: choice distribution).
```

The plugin owner matched the original native request and output locally.
The request invokes the reported v2.2.13 runtime.
Current production handler and validator source match that release.
The independent source reviewer received public code and synthetic controls, without the native incident payload.
No new local handling defect is established.

## Meaning and documentation

`missing_fact` is the question ID.
`choice distribution` identifies a failed consistency check on that question's answer.
The probabilities fail their sum check, or the selected choice lacks the highest probability.
Both conditions produce the same diagnostic. The message alone cannot distinguish them.

The validator already preserves the `malformed` category and the bounded question/check detail.
The hook retains both fields in metrics and returns an unverified result without an approach receipt.
The diagnostic omits response contents.
The README now explains this format and separates it from contract lookup diagnostics.
It also explains that the rewritten `printf` command can exit zero while printing an unverified result.

| Observed condition | Current behavior | Verification |
| --- | --- | --- |
| Probabilities total 1.6. | Return `malformed: missing_fact: choice distribution`. | Synthetic Codex and Claude hook tests. |
| Selected choice has probability 0.1 while another has 0.9. | Return the same diagnostic. | The same two host paths. |
| A disposable control changes the category to `unreachable`. | The new guard tests fail. | Four child failures and a failed parent. |
| A disposable control removes the detail. | The new guard tests fail. | Four child failures and a failed parent. |

## Verification

The new test uses the existing fake transport and existing hook fixtures.
It changes only the returned `missing_fact` distribution and adds a private-response sentinel.
Each case verifies one request, an unverified record, no receipt, preserved metrics and no sentinel in output or the record.
No request reaches a provider.

| Check | Result |
| --- | --- |
| Four malformed cases, existing malformed checks and a healthy Codex consultation | Nine test pass records, no skips or failures. |
| Full Jev and hook packages with race detection | 329 test pass records, 33 skips, no failures or detected race. |
| Two negative controls | Each exits 1 because all four new child cases fail. |
| Vet, formatting and whitespace | Pass. Exit status and output hashes are retained for each command. |
| Independent final source review | No confirmed bugs. Native metadata verifies GPT-6 Luna/max with read-only permissions. |

The reviewer reports an unsuccessful nested worker attempt. That attempt is excluded from the verification result.
The completed source review comes from the verified Luna/max process.

## Boundaries

Only tests and documentation change.
The reported SDK consultation is not replayed, and its request payload is not exported.
No production code, SDK source, builder, provider, policy or installer changes occur.
The installed runtime remains v2.2.13. No binary release or installation is needed.
These controls establish diagnostic preservation, not the cause or recovery of the reported service failure.

Restricted local evidence remains under ignored `probe/out/jev-diagnostics/malformed-choice-20261018/`.
