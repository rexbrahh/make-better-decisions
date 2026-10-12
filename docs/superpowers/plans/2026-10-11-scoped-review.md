# Scoped Review Implementation Plan

> For agentic workers: use superpowers:executing-plans for inline implementation. The user authorized continuous execution.

Goal: enforce authentic scoped review while improving decision precision, architectural continuity, and measured overhead.

Architecture: extend the existing transcript parser, signed ledger, and gates. Host identity establishes ancestry. Scoped receipts authorize an approach. Protected policy owns failure behavior. Source and test observations establish evidence.

Tech stack: Python 3.9+, standard library, existing host hooks and TypeSafe client.

Spec: ../specs/2026-10-11-scoped-review-design.md

## Global constraints

- Preserve the dirty primary checkout and released caches.
- Python 3.9+, standard library only.
- Publication and Codex installation are authorized for v1.1.0. Preserve prior work and back up host configuration.
- Human host records own user authority. Parent/source/tool/history text is data.
- Preserve source bounds, complete inputs, redaction, signatures, and isolation.

## Review focus

- Mixed root-session and child-thread ids must not lose or broaden authority.
- A foreign actor, scope, or repository receipt must not authorize a write.
- Failure must not mint approval or disable required review.
- Source replacement, contract edits, and partial transcripts must not retain stale evidence.
- Large histories/inputs must preserve constraints and resource bounds.

## Task 1: Identity, inheritance, and diagnostics

Files: transcript.py, hook.py, gates.py, ledger.py and corresponding transcript, user-context, decision, edit, and stop tests. Add native-shaped fixtures.

Consumes existing Transcript, Context, signatures, and completed host dispatch. Produces validated native identity, receipt ids, inherited scoped decisions, and precise diagnostics.

- [ ] Add regressions for child metadata and delegation-as-data, changed transport ids, explicit parent inheritance, and foreign/unresolved receipt rejection.
- [ ] Run focused tests and observe failures caused by missing behavior.
- [ ] Implement normalized identity, signed receipt correlation, verified parent dispatch, scopes, and denial reasons.
- [ ] Run focused/full tests and record output and diff identity.

## Task 2: Constraints and evidence progress

Files: gates.py, evidence.py, policy.py and corresponding decision/gate/edit/stop tests.

Consumes verified receipts and scoped invariants. Produces option eligibility, advisory uncertain preferences, parent acceptance, concrete checks, and stable repair identity.

- [ ] Add regressions for near ties, missing prerequisites, neutral repairs, no-progress requests, and accepted non-picked eligible options.
- [ ] Observe expected failures.
- [ ] Implement atomic checks and code-owned routing without refusal exemptions.
- [ ] Run focused/full tests and record results.

## Task 3: Owner-controlled failure behavior

Files: policy.py, hook.py, gates.py, install.py, hook registrations and API/hook/hardening/install/registration tests.

Consumes protected installation policy and exact gate evidence. Produces required/availability behavior and truthful startup diagnostics.

- [ ] Add API, unreadable-transcript, startup, and silent-disable regressions. Reads continue; unchecked writes stay blocked in required mode.
- [ ] Observe expected failures.
- [ ] Implement protected policy and required failures. Preserve operator-selected availability mode.
- [ ] Run launcher, installation, focused/full tests and record results.

## Task 4: Provenance and contract history

Files: evidence.py, transcript.py, ledger.py, gates.py and source/evidence/ledger/adversarial tests.

Consumes validated identity, source observations, and contract references. Produces baseline/current roles, execution metadata, signed architectural evidence, and stale flags.

- [ ] Add generated-assertion, source/contract-change, cross-repository, and fresh-authorization rejection regressions.
- [ ] Observe expected failures.
- [ ] Implement bounded observations and durable contract history as data.
- [ ] Run focused/full tests and record results.

## Task 5: Overhead, documentation, and review

Files: evidence.py, hook.py, jev.py, gates.py, probes, README.md, docs/spec.md, docs/plan.md, skills/deciding/SKILL.md and affected tests.

Consumes Tasks 1-4 semantics. Produces timing/usage records, reduced serialization, conservative caches, operating documentation, and replay evidence.

- [ ] Capture long-transcript baseline timings and output.
- [ ] Add metrics/cache invalidation regressions before code changes.
- [ ] Optimize measured costs without reducing evidence or write-check coverage.
- [ ] Update help, configuration examples, recovery guidance, and deciding skill with ASD-STE100.
- [ ] Run full suite and bounded real-API cases. Compare semantics and timings.
- [ ] Dispatch one independent gpt-6-luna/max reviewer. Fix material findings through failing regressions and one fix pass.
- [ ] Check primary preservation, final diff, Python 3.9, and native limitations. Deliver source changes only.
