# Jev failure metrics implementation plan

> **For agentic workers:** Use superpowers:executing-plans for this selected design. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Record the phase of future unreachable Jev calls in existing diagnostic metrics.

**Architecture:** Add optional phase metadata to `JevError`. The hook records only `request` and `response_body` in API metrics. Public errors and review behavior stay unchanged.

**Tech Stack:** Go 1.27.2 and the standard library.

**Spec:** `probe/out/jev-diagnostics/metrics-proposal.md`, explicitly selected by the owner on 2026-10-16.

## Global constraints

- Set Phase only for unreachable transport failures.
- Preserve public errors, deadlines, retries, body cleanup and all review gates.
- Preserve Jev as default and Snap as opt-in.
- Exclude raw errors, credentials, request contents and response contents from phase metrics.
- Preserve unrelated untracked audit files and the three held proposals.

## Review focus

- A response-body failure must retain cleanup and the `unreachable` public error.
- Timeout, HTTP and validation failures must have no phase label.
- Unknown phase values must not reach metrics.
- Phase metadata must not enter review state or questions.
- Required and availability policies must retain their existing outcomes on failure.

### Task 1: Implement and verify the selected metadata path

**Files:** `internal/jev/jev.go`, `internal/jev/jev_test.go`, `internal/hook/hook.go`, `internal/hook/jev_test.go`.

**Interfaces:** `transportError(err error, phase string) error` returns `*JevError`. The optional `Phase string` field is diagnostic metadata. `askerWithKey` copies allowlisted values to `api_calls[].error_phase`.

- [x] Add regressions for request/body failures, timeout/HTTP omission, body closure, public errors and hook metrics.
- [x] Run the tests before behavior changes. Observe missing metadata failures.
- [x] Add the field, two static call-site labels and metrics allowlist. Do not change error formatting.
- [x] Run focused tests, then the complete product suite with the frozen Python oracle and race detection.
- [x] Obtain independent GPT-6 Luna/max review of the diff and evidence. Resolve confirmed findings and repeat affected checks.

### Task 2: Publish and activate v2.2.8

**Files:** existing version manifests, binary pins and release validation document.

**Interfaces:** `scripts/build-release`, existing signed release workflow and protected installer for both hosts.

- [x] Update release metadata, build Darwin and Linux assets, and verify reproducibility and Linux behavior.
- [x] Create signed source commit and tag. Publish assets and independently verify downloaded hashes.
- [x] Update both plugin caches. Preview and apply both installations through the protected installer.
- [x] Verify registration, trust, active version and fresh native sessions in Codex and authenticated Claude Code.
- [x] Publish the exact checks, artifact identities and limitations in the validation document.
