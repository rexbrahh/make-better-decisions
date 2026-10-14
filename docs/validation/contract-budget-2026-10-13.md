# Required contract evidence validation

## Observed failures

| Issue | Correction | Evidence |
| --- | --- | --- |
| Field Store received repeated requests for Windows scripts already read | Preserve all required source records and shorten excerpts | Current-source replay of the six-file request retained four records before the correction and all six afterward. Both packaging scripts were previously missing. |
| Hosted received repeated requests for contracts already read | Use the same required-record retention in both hook trims | Current-source replay of the eight-file request retained five records before the correction and all eight afterward. The error header and two web contracts were previously missing. |
| A Python diagnostic was rejected as a compound consultation | Treat a complete quoted data heredoc as stdin data | Parser and hook regressions verify that diagnostic text does not construct context or call Jev. Negative cases retain enforcement for actual shell invocations. |
| Common wrappers hid a literal consultation, and the fallback called every unintercepted invocation an inactive hook | Detect common literal wrappers and correct the fallback message | Wrapper regressions first reproduced nine misses. Ordinary lookup and data cases pass. The fallback still exits 3, and old and new unverified messages cannot authorize an edit. |
| Two source-race tests did not mutate the reader's path on macOS | Resolve the test fixture root | The fixture used the `/var` spelling while the reader used `/private/var`. Both existing mutation tests pass with canonical fixture paths. |

## Replay method

Read native transcripts with the streaming loader. Extract the peer's literal consultation request. Build evidence against the current source tree without calling Jev or changing peer files. Independently compare each retained SHA with the complete source bytes. Add contract and file freshness metadata, trim again, and check that every required record remains within the budget.

Field Store retained six of six required records in 77,442 characters after freshness metadata. Hosted retained eight of eight in 75,603 characters after freshness metadata. These are current-source evidence replays, not historical execution receipts.

## Checks

All 370 tests pass under macOS system Python 3.9.6 and Homebrew Python 3.15. The final runs were sequential. An earlier parallel run reached 1.546 seconds against one test's 1.5-second timing limit; the complete system suite passed when run alone. The limit was not changed.

Independent review reproduced wrapper misses and verified the corrections. Its final negative authority probe rejected the new unverified fallback output. Source-race, redaction, evidence-budget, native identity, and signed-review regressions pass. The diff passes the whitespace check.

## Limits

No Windows build, replay, native promotion, or deployment ran for these checks. No peer exception was added. The existing SDK exception remains owner authorized. The Hosted peer reported two passing Go event-copy tests; this repair did not run those tests or approve that patch.

The command classifier recognizes the documented standalone format and common literal wrappers. It does not evaluate arbitrary shell programs, aliases, or dynamic substitutions. An unintercepted call cannot generate a reviewed receipt; the fallback reports an unverified decision and exits 3.
