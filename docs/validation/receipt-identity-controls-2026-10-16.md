# Receipt identity regression controls

The receipt test now changes only the provider name in its provider-mismatch case.
Production code already compares the complete identity. This round strengthens regression coverage and repairs no runtime defect.

## Isolated identity fields

The previous test selected the Jev preset, which changed provider, model and context profile together.
Those other differences could hide an omitted provider comparison.
The tightened case keeps model and profile fixed while changing `Reviewer.Name`.
This synthetic tuple isolates the comparator field. It does not configure a review provider.

The existing test retains matching-identity and legacy controls for completed and inherited receipts.
Four disposable Go overlays omit one identity field from both sides of the production comparison.
The overlays never replace installed code or modify the production source file.

| Omitted comparison field | Previous test | Tightened test |
| --- | --- | --- |
| Provider | PASS: omission missed | FAIL: omission detected |
| Model | FAIL: omission detected | FAIL: omission detected |
| Context profile | FAIL: omission detected | FAIL: omission detected |
| Review contract | FAIL: omission detected | FAIL: omission detected |

Every expected failure reaches the receipt-count assertion: `got 2 receipts, want 0`.
The failing cases compile successfully. The unchanged production matcher passes the tightened test.

## Verification

| Check | Result |
| --- | --- |
| Mac gate suite with race detection and CPython gate oracles enabled | 100 test records pass. Seven paid live suites skip. |
| Formatting and static checks | `gofmt -d`, `go vet ./internal/gates` and `git diff --check` pass. |
| Independent source review | GPT-6 Luna/max reports no finding. Native metadata verifies the requested model and effort. |

The reader inspects the packet and public gate helpers. It does not rerun tests or independently establish run freshness.
Root retains the raw Go event streams and checks each precise failure assertion.
The reviewer thread is `01a146e7-dd6a-7ce1-9e6f-70eaa232c58a`.

Only `internal/gates/review_test.go` and this validation record change.
The installed engine remains v2.2.8. Its binary SHA-256 remains `aa00d37db169016942f215440962cea55cff44dc4ec88a72df93949a591908d8`.
No provider policy, installation, SDK source or builder changes occur.
The separate middle-contract proposal remains held for owner selection.

Raw controls, mutation overlays, source hashes, reviews and event streams are retained under ignored `probe/out/subq-followup/identity-field-controls-2026-10-16/`.
