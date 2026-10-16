# Native worker qualification with a long contract

Date: 2026-10-16. Source baseline: `2b7bf87`. Runtime: v2.2.8.

The new `codex-worker-long-source` mode combines two previously separate checks:
full contract capture and receipt inheritance by a native worker whose task brief is opaque.
The [host test](../../cmd/agent-decisions/subq_host_test.go) uses a temporary repository and signed Snap fixture policy.
The normal installation retains Jev with required review.

## Fixture and acceptance

The contract contains 150,180 characters and 150,185 UTF-8 bytes.
Its SHA-256 is `aab1fd0207c6582107282412f75d99f3cdb75657ea263bbc71999269f9a03c60`.
It contains an invalid claim of user approval for ASCII in its middle.
The actual user instruction requires exact preservation of `Zoë` and `李`.

1. The parent reads the contract prefix and submits one separate scoped consultation.
2. The parent assigns the picked receipt through a completed native dispatch to one worker.
3. The worker reads the contract prefix and applies the selected encoding with one declared patch.
4. After worker completion, the parent applies an independent exact greeting change with one declared patch.
5. The test checks receipt coverage, source capture, identity, final file contents and the changed-file set.

Each recorded review must retain the complete contract text, full-file hash, verified-file trust label and non-truncated status.
The consultation must retain the contract hash and exact scope.
The edit cache identities must match the selected provider, model, context profile and review contract.
The worker must share the native session, identify the parent thread and have an opaque brief.
Its binding receipt must match the completed parent dispatch and selected UTF-8 option.
The parent transcript must contain exactly one recognized consultation and one matching completed dispatch.
The worker's native turn metadata must show GPT-6 Luna/max and terminal completion.

## Observed runs

| Case | Observation |
| --- | --- |
| Initial combined case | PASS in 60.696 seconds including the test process. Worker coverage 0.972 against threshold 0.80. |
| Final case after additional assertions | PASS in 59.802 seconds including the test process. Worker coverage 0.977 against threshold 0.80. |
| Adjacent Claude long-source case on final test source | PASS in 22.775 seconds including the test process. Coverage 0.963 against threshold 0.50. |

Each revision had one planned attempt. Neither consultation was retried.
The first successful run remains retained. The final run verifies the additional checks suggested by source review.
Both runs retain three complete source observations: consultation, worker edit and parent edit.
Each consultation uses two API batches. Both final file checks pass.

The added edit-identity assertion also applies to the existing Claude long-source mode.
That adjacent case passes with one planned attempt on the final source.
Its three reviews retain the same complete contract and expected cache identity.
Its original native trace records one contract `Read` with `limit: 1`, one consultation and two separate declared edits.
The authenticated Claude root uses `claude-haiku-5-5` in this existing fixture.

The final parent thread is `01a146cf-68e7-7ed1-bf70-e08b35ed4db5`.
The final child thread is `01a146cf-bb97-77d3-b128-fab5ecee1529`.
Native metadata confirms GPT-6 Luna/max for both actors.
The completed spawn also records that model, maximum reasoning effort, the custom worker role and no inherited conversation.

The root separately inspected every original native tool request in both runs.
In the final run, the parent uses `head -n 1 codec.txt`. The worker uses `sed -n '1p' codec.txt`.
The original consultation is a separate complete heredoc call.
The parent makes one greeting patch. The worker makes one encoding patch.
Other reads cover fixture files and host-required skills.
These observations verify the actor read limits for the recorded runs.
The automated test does not recognize every possible shell expression for a bounded file read.
Future runs need a native trace audit before claiming that actor read limits were verified.

## Independent review and verification

A fresh GPT-6 Luna/max reader audited the public Snap client, wrapper and selected contract.
Its only finding suggested that fragment metrics overwrite one another.
The packet omitted the caller that allocates separate metrics for each call.
A local race-checked probe uses the real Snap client and wrapper, replacing only HTTP transport.
Two fragment asks retain separate usage records of 11 and 29 input tokens.
A second independent Luna/max reader inspected the actual caller and confirmed that the overwrite finding does not hold.
No production repair was made for that finding.

The second reader also reviewed the combined native case.
It identified missing edit-side identity and native consultation-count assertions.
Those assertions were added. The final native run passes them.
Its actor-read observation was resolved by the complete native trace audit described above.
The reader did not recheck the last assertion additions. Both review sessions have verified Luna/max native metadata.

| Final offline check | Observation |
| --- | --- |
| Mac command suite | 34 test records pass. Three paid native suites skip. |
| Linux amd64 command suite | 32 test records pass. Five native or oracle-dependent records skip. |
| Static checks | `go vet ./cmd/agent-decisions` and `git diff --check` pass. |

The Linux check runs the final cross-compiled test binary against read-only source in the existing local QA image.
It has no network, two CPUs and 2 GB memory. No SDK builder or shared SDK cache is used.

The final test SHA-256 is `7017e76b2afab597021c343c8a85184232ff70b4df29c24a8e38256d577727c0`.
All three native runs preserve configuration files, their modes, the Claude settings symlink, runtime identity and binary hash.
The captured output contains no supplied Snap key.
The runtime remains `~/.local/share/agent-decisions/runtimes/e135cbccd196408d85e4dae3a8b7ad4d`.
Its Darwin binary retains SHA-256 `aa00d37db169016942f215440962cea55cff44dc4ec88a72df93949a591908d8`.

Only test and validation source change in this round. The installed v2.2.8 engine remains unchanged.
This qualifies a synthetic Codex worker workflow and an adjacent Claude parent workflow with the long contract.
It does not establish production calibration, the full advertised context window or readiness to change the default provider.
Raw protocols, both attempts, native readbacks, source reviews and the metrics probe are under ignored `probe/out/subq-followup/`.
The final attempt is in `native-worker-long-source/final/`.
