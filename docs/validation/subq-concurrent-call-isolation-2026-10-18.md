# Snap failure isolation across concurrent calls

Date: 2026-10-18 UTC. Source baseline: `2dcbc4a4646404eff27523284982da715dfd9038`.

The client shares request slots across calls and creates a separate cancellation context for each call.
Existing tests check concurrent success, failed-batch cancellation and sequential client reuse.
The new test checks a failed call while another call remains active on the same client.
This closes a coverage gap. No production defect is confirmed.

| Coverage gap | Added check | Evidence |
| --- | --- | --- |
| The existing suite does not detect cancellation of an overlapping independent call. | Fail one call while holding another call open, then release the independent call. | A disposable cross-call cancellation fault passes the existing suite and fails the new test. |

The fixture starts a 16-question call and holds its transport response with a channel.
A second call receives a malformed response that omits its requested answer.
The second call must return no answers and record no validated model.
Its cancellation must leave the first call's context active.
The first call must remain blocked until release, then return all 16 answers and its validated model.
Both response bodies must close. Exactly two transport calls must occur.

The fixture uses the existing simulated transport and a dummy key.
It joins the background caller during cleanup, including after a failed assertion.
Channels control the order. The fixture does not use sleeps.

| Check | Result |
| --- | --- |
| Twenty Mac race-instrumented repetitions | 20 pass, no skips |
| Complete Mac Snap package with race detection | 54 named test pass records, four opt-in live skips, no failures |
| Complete Linux amd64 Snap package without race detection | 54 named test pass records, four opt-in live skips, no failures |
| Existing suite with an injected cross-call cancellation fault | 53 named test pass records, four opt-in live skips |
| New test with that same fault | Expected failure: the failed call cancels the independent call |
| Vet and whitespace checks | Pass |
| Independent GPT-6 Luna/max source review | No actionable finding |

The fault exists only in an ignored Go overlay.
The Linux container uses read-only source, the existing local toolchain and no external network.
Native turn metadata verifies the review model, reasoning effort and read-only sandbox.
The reviewer inspects the new test, client source and retained logs. It does not run tests.
Production client SHA-256 remains `a549038bb4a05050677649fd5d21e11bd094694f9983329f8a73398d38f9164e`.
Only test and documentation files change. No runtime release or installer change is required.
These checks do not establish live provider connectivity, TLS behavior or production calibration.
Raw results remain under ignored `probe/out/subq-followup/concurrent-isolation-20261018/`.
