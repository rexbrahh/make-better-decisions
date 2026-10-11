# Current acceptance plan

Date: 2026-10-11. Scope: v1.0.1 documentation and acceptance.

The old implementation recipe was replaced by this short plan. Git history keeps the original recipe.

## Required checks

1. Run the unit suite.
2. Run focused adversarial tests for facts, source evidence, edit gates, stop gates, and isolation helpers.
3. Run live Jev acceptance with the real key.
4. Check native install planning with `agent-decisions-install --help` and a dry-run plan.
5. Install the native plugin, trust five hooks, then apply the boundary helper from the installed root.

## Configuration checks

1. Save backups before writing host config.
2. Reapply the helper after stdio MCP or plugin command changes.
3. Treat `source: null` as a refresh limit.
4. Do not treat preserved repeat recipes as update tracking proof.
5. Record macOS boundary evidence and unverified limits.

## Acceptance points

- Optional facts accept normalized user excerpts only.
- User excerpts are not independent runtime-truth proof.
- Source evidence reports hashes, origins, trust labels, omissions, and truncation.
- User history accepts prompt-submit records only after a new human-origin transcript match.
- Decisions need signed ledger entries and full verdict receipts.

More acceptance points:

- Later user instructions override earlier binding decisions.
- `REFUSED`, `GATHER EVIDENCE`, and Stop checks have no exemption path.
- `PROCEED` covers exact or routine work only.
- Edit gates reuse relevant decisions across turns.
- Stop accepts final choices only at `uncovered_choice <= 0.2`.

Boundary acceptance points:

- Large tool inputs keep complete field identity through chunking.
- Opaque future executable stdin is denied while healthy.
- MCP write gates and native deny profiles are covered.
- Stdio MCP servers are wrapped.
- Approved fail-open remains visible.

## Known limits

- Do not claim final live counts or global activation until parent evidence records them.
- Linux bubblewrap behavior is not runtime verified here.
- Codex GUI identity boundaries are not verified here.
- Codex subagent identity boundaries are not verified here.
- Same-UID privileged processes and trusted external services can bypass hook-only controls.
