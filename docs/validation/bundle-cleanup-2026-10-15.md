# Failed runtime publication cleanup

## Observed failure

`Publish` creates its stage before it registers deferred cleanup.
An RNG failure during pointer naming leaves that stage.
After a successful rename, cleanup still refers to the old stage path.
A failed pointer activation therefore leaves the new bundle.

| Issue | Correction under evaluation | Evidence |
| --- | --- | --- |
| Pointer naming fails after stage creation | Register cleanup immediately after stage creation | The injected RNG failure leaves `.stage-aaaaaaaa` before the fix |
| Pointer activation fails after bundle rename | Track the new bundle until activation succeeds | The injected activation failure leaves two runtime directories before the fix |

## Candidate and checks

The isolated candidate is in ignored `probe/out/v21-campaign/round4-candidate/`.
It registers cleanup before pointer naming, tracks the renamed bundle and clears that ownership after activation.
The existing cleanup helper accepts empty paths for artifacts that do not exist or are already active.
It removes only paths created by the current attempt.
It does not prune previous runtimes.

The new regression injects failures during pointer naming, bundle naming and activation.
Before the fix, pointer naming and activation fail the artifact-count assertion.
After the fix, all three cases and the complete bundle package pass.
The strengthened regression first publishes two successful bundles.
Each injected failure preserves the active pointer, both runtime directory names and both executables.
It leaves no new stage, bundle or temporary pointer.
Existing success, source-integrity, mode and symlink checks also pass.

The reviewed source diff changes only `Publish` and its private `cleanup` helper in `internal/bundle/bundle.go`.
`Publish` supplies cleanup paths from its own successful stage creation, unique pointer name and successful bundle rename.
No previous runtime enumeration or pruning is introduced.
No hook, review, isolation or ledger implementation changes are included.

Raw results: `probe/out/v21-campaign/bundle-cleanup-before.log` and `bundle-cleanup-candidate.log`.
The candidate has not yet been applied to production at this measurement point.
