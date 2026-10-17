# Snap malformed-batch cancellation checks

Date: 2026-10-17. Source baseline: `cb650944cc811429a8ac7731b11ebb82ee509ead`.

The existing tests cover malformed replies and parent cancellation separately.
The new test combines a completed batch, a malformed batch and a blocked sibling.
This closes a coverage gap. These checks find no production defect.

## Fixture

The [Decisions API](https://docs.subq.ai/endpoints/decisions-api/) accepts at most 16 questions per request.
The existing client splits 33 questions into three batches.
The fixture uses the existing response helpers and a simulated transport with a dummy key.

| Batch | Controlled state | Required result |
| --- | --- | --- |
| First | All 16 answers validate and its body closes. | These answers cannot become partial output. |
| Second | Its response omits one requested answer after the first batch completes and the third starts. | Ask returns `malformed` with no answers. Its response body closes. |
| Third | Its transport waits for its request context to end. | It observes cancellation, not deadline expiry, before Ask returns. |

The incomplete operation cannot record a validated returned model.
The one-second fixture timeout bounds a failed test. Production deadlines remain unchanged.

## Verification

| Check | Result |
| --- | --- |
| Twenty Mac race-instrumented repetitions | 20/20 pass, no skip |
| Complete Mac Snap and hook race suites | 308 test passes, two package passes, 31 test skips, no failure |
| New test in Linux amd64, non-root, external network disabled | One test pass and one package pass, no skip |
| Remove sibling cancellation through a disposable Go overlay | Fails because the sibling reaches its deadline |
| Omit failed-response body closure through a disposable Go overlay | Fails the body-closure assertion |
| Return a completed batch after another batch fails through a disposable Go overlay | Fails with 16 partial answers |

Vet, formatting and whitespace checks pass.
The overlays leave production client SHA-256 `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e` unchanged.
The Linux container uses the existing local image and toolchain, with read-only source and no external network.

An independent GPT-6 Luna/max source review finds no actionable defect.
Native turn metadata confirms the model and reasoning effort.
The reviewer inspects the fixture, client, selected contract and retained fault results. It does not run tests.
The first subagent dispatch fails before review because that dispatcher substitutes an unsupported model.
The completed review runs through the installed Codex CLI with explicit Luna/max settings.

The final test file has SHA-256 `f6c1e8de10ca10363a978d5ae44ebd79640563ae1f8ecb2015f6c0a6ff25e526`.

One declared documentation update fails as `jev unreachable` before writing.
The original native request and output match that failure.
An existing synthetic Jev control then passes with one API call in 0.36 seconds, and the exact documentation patch succeeds.
This establishes later operation recovery. It does not establish the original backend cause or a service repair.

Only test and documentation files change. No binary release or installer change is required.
These checks do not establish real provider connectivity, TLS behavior or model judgment quality.
Raw results and fault overlays remain under ignored `probe/out/subq-followup/batch-failure-20261017/`.
