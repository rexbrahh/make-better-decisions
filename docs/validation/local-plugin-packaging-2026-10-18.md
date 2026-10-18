# Local Codex plugin packaging

Date: 2026-10-18 UTC. CLI: `codex-cli 0.163.0`.
Source baseline: `fb6d32854499103567260d8c3b3d68572921e203`.

The previous README instructed users to install directly from a local checkout.
The historical [v2.2.13 QA record](v2.2.13-qa-2026-10-17.md) records 28,959 extra files in the initial Codex copy.
That record verifies the corrected release cache against its signed source archive.
This check reproduces the packaging behavior with synthetic files and corrects the instructions.

| Issue | Correction | Evidence |
| --- | --- | --- |
| A local checkout includes Git metadata and local data in the installed copy. | Stage committed source with `git archive` before local installation. | The native CLI copies `.git/`, a Git-ignored sentinel and an untracked sentinel. The archive installs exactly four committed files, with zero extras. |
| An existing marketplace name rejects a different source. | Mark the recipe as a fresh registration. | Adding the archive to the populated fixture home exits 1. Its existing cache retains all 33 extra files. |
| The Mac's Nix launcher supplies a runtime profile to plugin commands. | Document its existing `CODEX_CLI_FULL_SKILLS=1` opt-out. | The wrapper rejects `plugin marketplace list --json`. The native executable and the existing opt-out both complete that read-only command. |

## Controlled installation

The fixture contains two manifests, one tracked text file and `.gitignore`.
A local Git commit records those four files.
The fixture then adds one ignored file and one untracked file.
No personal transcript or real repository data enters this fixture.

Two separate temporary Codex homes receive the working checkout and its Git archive.
Each case uses supported `plugin marketplace add` and `plugin add` commands.
Both complete successfully. The working copy includes local data. The archive matches all four committed files without extras.
No host installer or model request runs. Provider keys are removed from the child environment.
The real host marketplace listing and active runtime pointer match their pre-check values.
A separate read of the installed binary reports v2.2.13, retained in `host-version-proof.json`.

The first probe stops before installation because the Nix launcher adds `-p cli`.
The completed packaging probe invokes the same installed CLI directly through `/opt/homebrew/bin/codex`.
The wrapper opt-out is checked separately against the read-only host marketplace listing.
`wrapper-existing-optout-v2.json` records the exact command, `CODEX_CLI_FULL_SKILLS=1` override and launcher guard.
`wrapper-error-proof.json` retains the default wrapper's profile error, launcher path, source hash and argument-injection line.
No Nix source or live launcher changes occur.

The README now gives a Git archive recipe and states that the staging directory must remain while its marketplace is configured.
It also states that uncommitted changes do not enter the archive.
An additional fixture check attempts to replace its already registered source with the archive.
Codex rejects that request before plugin installation. The recipe therefore requires a fresh registration.
This check does not migrate an existing registration or clean an existing plugin cache.
No file-exclusion mechanism was found in the [official packaging page](https://developers.openai.com/plugins/build/plugins).
That page documents local and Git marketplace sources and the installed cache.
The observed copy behavior is specific to the tested CLI version.

Only documentation changes. No binary release or host reinstallation is required.
An independent GPT-6 Luna/max source review identified the registration prerequisite and missing evidence references before publication.
The final read-only review found no recipe defect and identified two remaining evidence-reference gaps.
The records now retain the exact wrapper override and native model metadata.
`reviewer-model-proof.json` and `review-final-model-proof.json` retain the verified model and effort.
The reviews run no tests or installations.

A resumed follow-up selected workspace-write instead of read-only.
It was interrupted before any source command. A fresh review completed with verified read-only permissions.
The interrupted run and its metadata remain retained. This was a review-launcher error, not a plugin defect.
Raw commands, synthetic fixtures and results remain under ignored `probe/out/local-packaging-20261018/`.
