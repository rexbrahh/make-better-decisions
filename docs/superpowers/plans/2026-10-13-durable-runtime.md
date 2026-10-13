# Durable Runtime Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox syntax.

**Goal:** Preserve native communication and required review across plugin cache removal.

**Architecture:** Stage complete protected runtime bundles and atomically activate a stable pointer. Restore signed native launcher recipes while retaining third-party isolation.

**Tech Stack:** Python standard library, host hook registrations, native config RPC.

**Spec:** ../specs/2026-10-13-durable-runtime-design.md

## Global Constraints

Preserve required review, existing permission defaults, signed recipes, and unrelated configuration. Retain earlier runtime bundles. Reject source symlinks.

## Review Focus

Tests cover dangling pointers, interrupted publication, missing files, escaping symlinks, and native transport metadata.

## Task 1: Runtime and migration tests

Files: tests/test_runtime.py, tests/test_install.py, tests/test_registrations.py.

- [x] Add tests for stable launch paths, namespace protection, native preservation and signed restoration.
- [x] Add tests for cache deletion, partial publication, source links, and broken installed runtime.
- [x] Run `TMPDIR=/private/tmp python3 -B -m unittest tests.test_runtime tests.test_install tests.test_registrations -q`. Observe expected failures.

## Task 2: Runtime publication and installer integration

Files: agent_decisions/runtime.py, agent_decisions/install.py, codex/hooks.json, hooks/hooks.json.

Interfaces: `runtime.namespace(home=None) -> Path`, `runtime.current(home=None) -> Path`, `runtime.publish(source, home=None) -> Path`.

- [x] Copy verified allowed files into a unique retained bundle. Atomically activate `current`.
- [x] Add namespace protection and stable launch paths to both host plans. Publish before signing and applying configuration.
- [x] Restore native transports from signed recipes. Leave unwrapped native registrations unchanged.
- [x] Make registered hooks select the installed runtime before cache code.
- [x] Run targeted tests, then the full suite with canonical TMPDIR and native sandbox permissions.

## Task 3: Release and activation

Files: README.md, plugin manifests, docs/releases.

- [x] Document owner-controlled runtime updates and initial-install fallback.
- [ ] Review the focused patch, commit, and publish the authorized release.
- [ ] Activate the runtime and migration with backups. Preserve old live hook paths during cache refresh.
- [ ] Verify installed hooks, protected paths, and native read-only communication.
