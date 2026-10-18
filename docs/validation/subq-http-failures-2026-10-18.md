# Snap HTTP failure regression coverage

Date: 2026-10-18 UTC.

This change adds missing regression coverage for Snap HTTP failures.
The production adapter already rejects these responses. No new production defect is claimed.

The endpoint, model and 16-question batch limit match the current [SubQ Decisions API](https://docs.subq.ai/endpoints/decisions-api/).
The documentation lists credential, credit, permission, validation, rate-limit and server errors.
The previous offline Snap tests covered a redirect, but not these service errors.
An opt-in live test also checks an oversized-context 400 response.

## Scope

`TestAskRejectsHTTPFailuresBeforeReadingAnswers` covers nine statuses:
302, 400, 401, 402, 403, 422, 429, 500 and 503.
Each status has two fixtures: an otherwise valid answer and an opaque error body.
Both use the existing fake transport and response helpers.
The 302 fixtures include `Location`, so Go's redirect handling also runs.
All fixtures use a test key. They make no external request.

Each case checks that the client:

1. Returns no answers and reports exactly `http_<status>`.
2. Consumes zero response bytes and closes the response body.
3. Makes one request.
4. Removes a stale `returned_model` metric.

This is a representative failure matrix, not an exhaustive HTTP status census.
The existing redirect test still checks credential non-forwarding.

## Verification

| Check | Result |
| --- | --- |
| Complete affected package on Mac with race detection | 40 test pass records, including all 18 new cases. Four live tests skip. No failures. |
| Complete affected package in the existing Linux amd64 container | 40 test pass records, including all 18 new cases. Four live tests skip. No failures. |
| Controlled fault: accept 4xx and 5xx responses | Prior offline tests pass: 21 records and four live skips. The new matrix rejects the fault in all 16 service-error cases. |
| Controlled fault: close only successful response bodies | All 18 new cases fail at the body-closure assertion, including both 302 cases. |
| Vet, formatting and whitespace checks | Pass |
| Final independent GPT-6 Luna/max source review | No confirmed findings. Native metadata verifies the model, effort and read-only sandbox. |

Fault controls use disposable Go overlays. They do not edit production source.
Their failed test processes are expected negative results, not production regressions.
The result parser requires terminal package records and checks the exact case counts.

Two earlier review passes found test-fixture omissions.
The first identified missing redirect lifecycle coverage.
The second identified the missing `Location` header in the added 302 fixture.
Both are corrected before the final Mac, Linux and source checks.

The first Linux launch omits the fixture's Go toolchain mount and exits before testing.
The corrected launch restores the retained read-only mount. The final run passes.
The container uses the existing pinned image, user `65534:65534`, no network and read-only source.
It has two CPUs and a 2 GiB memory limit. No image is pulled.

## Peer evidence and boundaries

The audit peer supplied its retained command and completed native output.
Root independently verifies seven identity/profile/diagnostic tests and eight diagnostic subtests.
Those tests are not rerun. Current provider and gate source bytes match v2.2.13.
These retained results do not establish calibration or workflow benefit.

The production adapter SHA-256 remains `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.
The final test file SHA-256 is `61d74041f937233e1c575acd90146776a3dc0abbfe1d56f18d3638cbbbac1a9f`.
The installed runtime remains v2.2.13 and matches its published Darwin binary hash.
Only the test and this document change. No binary release or installer action is required.

These checks establish local HTTP failure handling. Live provider connectivity, production calibration and intermittent Jev failure causes remain unverified.
Raw logs, overlays, review records and hashes remain under ignored `probe/out/subq-followup/http-failures-20261018/`.
