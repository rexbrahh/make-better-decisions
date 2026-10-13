# Selective review validation

The owner changed the product direction on 2026-10-13: ordinary tools should operate normally, and a few decision-sensitive tools should require consultation.

## Observed blockers

| Issue | Repair | Evidence |
| --- | --- | --- |
| Ordinary reads and peer status required Jev | Default ordinary tools to host execution | An outage probe allowed `Read` but denied `git status` and `wait_threads` before the change. The new tests require zero API calls for ordinary and unknown operations. |
| Completion depended on remote review | Remove automatic completion review | A findings-only reviewer reply was blocked. Legacy completion callbacks now return without context or API access. |
| A standing SDK exception did not defeat a semantic edit denial | Store an owner-signed session exception | The SDK transcript contained a standing human exception and a later patch refusal despite 12 eligible receipts. Tests cover owner precedence, native worker identity, payload mismatch, tampering, and revocation. |
| Peer tools were missing from this chat | Report the exposure gap separately | Current tool inventory contained no native read/send chat tools, while desktop startup logs reported the server ready. Transcript inspection supplied observations. No fresh peer report was received by messaging. |

## Boundaries

The private-state guard runs before dispatch. The edit and explicit decision implementations, signed receipt validation, native launcher ownership, and host permissions remain active. The owner session exception does not fabricate a Jev decision and does not exempt explicit consultations.

The installer writes exceptions through the existing signed policy. An upgrade preserves them. Explicit owner removal restores required execution review.

## Checks

The new tests failed before implementation. The full suite passed 363 tests under system Python and Homebrew Python with native sandbox permissions. Independent review identified and resolved cross-host exception scope and stale contract text. Installed-runtime checks are recorded in the delivery report after activation.
