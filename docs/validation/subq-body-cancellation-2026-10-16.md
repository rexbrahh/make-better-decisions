# Snap response-body cancellation checks

Date: 2026-10-16. Source baseline: `95eee83`. Installed runtime: v2.2.6.

## Coverage

The existing Snap cancellation tests block inside a simulated transport.
The new [transport test](../../internal/subq/transport_test.go) exercises body reading over local HTTP after response headers arrive.
It uses the existing client and response helpers with a dummy key.
Only the fixture request clone changes its destination to loopback.

| Condition before cancellation | Trigger | Required result |
| --- | --- | --- |
| One batch has validated and closed its body. A second body has returned data while the server holds the remainder. | Explicit parent cancellation | No answers, `cancelled`, both bodies closed, client workers joined and server handler stopped |
| The same two-batch state | Client deadline expiry | No answers, `timeout`, the same cleanup |

The setup timer starts before Ask starts and permits two seconds for both readiness signals.
Each accepted signal must also precede the setup deadline, even if its channel and the timer are both ready.
The second signal follows an underlying body read that returns data.
The deadline fixture uses three seconds. The result guard permits four seconds.
Production deadlines and all existing timing assertions remain unchanged.

## Verification

| Check on final test source | Result |
| --- | --- |
| Complete Mac Snap unit suite | PASS, including both new cases without skips |
| Ten race-instrumented repetitions of each new case | 20/20, no skips or race report |
| Linux amd64, non-root, external network disabled | Both new cases pass without skips |
| Remove request context in a disposable copy | Both cases fail at the body-reading return assertion |
| Remove all explicit response-body closure | Cancellation case fails its readiness assertion |
| Remove closure only for canceled responses | Cancellation case fails its closure assertion |
| Restore the disposable client | Exact baseline SHA-256 restored; positive control passes |

The negative controls change only a tracked-source disposable copy plus this new test.
They identify four expected failed cases across three injected faults.
Each failure remains recorded with its command, assertion and source hash.
Restoration matches client SHA-256 `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.

The Linux fixture uses the existing local image and a separate read-only Go 1.27.2 toolchain mount.
The cached toolchain archive matches its recorded SHA-256, and its Go binary matches the archive.
The container has two CPUs, 2 GB of memory and read-only source.
The first two runner attempts did not start tests because the toolchain mount was missing.
Both failures remain recorded. The completed run uses the verified mount and passes.

## Review

An independent GPT-6 Luna/max reader identified a timing risk in the first test draft.
The deadline could expire during its longer setup window.
The final draft starts one shorter setup timer before the Ask goroutine and gives the deadline a later expiry.
Final records include the negative-control assertions and restoration hashes that were absent from the initial summaries.

A second independent GPT-6 Luna/max reader identified late selection of a readiness signal, an early read signal and an ambiguous failure message.
The parent corrected those findings with a time comparison, a signal after received bytes and separate signal names.
The complete checks above pass again on that corrected source.

Both native reader records confirm the requested model and max effort. The readers did not run tests or review the later corrections.
The parent records the source-based resolution separately. Original review outputs remain unchanged.

## Scope

The final test SHA-256 is `c7e885e4061fcf17cff75be9a2fb9d715f975754b5e2be7b832c0d84da587c8d`.
Only test and documentation files change. No production defect was observed in these controls.
Production model requests, provider selection, policy, thresholds and receipt behavior remain unchanged.
No installer action occurs.
The installed v2.2.6 runtime still matches Darwin SHA-256 `637af1cd55219f63ee479e6b65adb2a8900b6367ad7e8b5294afecf68b694963`.
No new binary release or reinstallation is needed.

These checks cover loopback HTTP cancellation. They do not test provider connectivity, TLS or model judgment quality.
Raw attempts, commands, hashes, reviews and results are under ignored `probe/out/subq-followup/body-cancellation/`.
The final source checks are in its `final-v2/` directory. Earlier drafts and runner failures remain available.
