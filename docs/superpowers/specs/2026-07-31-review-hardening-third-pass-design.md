# Restore and Packstore Review Hardening Design

## Goal

Resolve the three findings from the combined review at `ccf293a` without
weakening restore confinement, physical-replica fallback, publication
cancellation, or multipart cleanup guarantees.

## Design

### Confine `BeforePublication` to private scratch

`RestoreOptions.BeforePublication` will keep its existing callback signature,
but `RestorePublicationTarget.DBPath` will identify a database inside a fresh
Kit-owned `0700` scratch directory under the repository staging directory.
The callback will never receive a database path whose resolution depends on
the mutable restore target namespace. `TargetDir` remains available as
application context, but it does not grant authority over the staged database.

Immediately before the callback, restore will copy the database currently held
through the target's pinned `os.Root` into private scratch. The callback must
close and checkpoint the database before returning and must not leave SQLite
`-wal`, `-shm`, or `-journal` sidecars. After a successful callback, restore
will reject a missing, symlinked, or non-regular scratch database and reject
any remaining sidecar. It will open the validated regular file, confirm the
opened identity matches the inspected identity, and copy the closed result
through the held root with `stageRootDatabase`. That existing helper supplies
an unpredictable target staging name, bounded context-aware copying, exact
length validation, file sync, and close before returning.

Once the new rooted staging file exists, restore will remove the old rooted
staging file and update `tmpRel` and `restoreState.dbRead` together before the
existing integrity/statistics proof and atomic publication. Cleanup will
remove private scratch on every path and join cleanup failures into the
returned error. Copying rather than renaming supports cross-volume scratch and
Windows. The callback-lifetime pathname contract remains source-compatible;
callers may no longer assume `DBPath` is beneath `TargetDir`.

This narrowly detours only the application callback. Moving the entire restore
and packed-catalog workflow into scratch would reduce other pathname reopening
but is a broader architectural change. Replacing the callback with a borrowed
`*sql.DB` would also be more invasive and would not by itself remove pathname
resolution from the configured SQLite opener.

### Classify pack index/footer disagreement as physical corruption

`Store.acquirePackedEntry` will wrap both a missing footer entry and a catalog
index/footer metadata mismatch with `ErrContentMismatch`. The filesystem
backend's existing physical-error classifier will then add
`ErrPhysicalCorrupt`, allowing multi-location resolution to try the next
authorized candidate and health tracking to demote the bad generation.

These failures are corruption, not physical absence: the canonical pack was
opened successfully, but its authenticated footer cannot satisfy the catalog's
claimed entry. Existing real source-not-found paths remain classified as
`ErrPhysicalMissing` and retain their one-time migration refresh behavior.

### Bound S3 source reads and abort cleanup

S3 repair and multipart publication will read caller sources through a local
context-checking reader matching the established packstore pattern. Repair
will check cancellation after its staging copy. Multipart publication will
check cancellation immediately after every `io.ReadFull`, before treating EOF
as success, hashing bytes, or issuing an upload request. This stops draining
between reads and prevents publication work after a source cancels while
returning its final bytes. A blocked arbitrary `io.Reader` cannot be forcibly
interrupted; the contract remains cancellation between reader calls and before
subsequent work.

Failed multipart publication must still attempt cleanup after caller
cancellation. The abort will therefore use a context derived from
`context.WithoutCancel(ctx)` with a package-owned ten-second timeout created
inside the deferred cleanup, so upload time does not consume the cleanup
budget. The timeout context preserves request values, is always canceled after
the abort call, and keeps the existing joined-error reporting. No public timeout
option is added.

## Testing

Focused regressions will prove:

- `BeforePublication.DBPath` is outside `TargetDir`, a callback mutation
  survives copy-back and proof, and a target namespace swap cannot redirect
  the callback into an attacker-selected database;
- callback failure, invalid scratch replacement, and leftover SQLite sidecars
  prevent publication and clean private staging;
- missing footer entries and index/footer mismatches fall through to a healthy
  replica for `OpenStream`, `Open`, and `ReadBounded`;
- cancellation during S3 repair staging stops further source reads and prevents
  the repair PUT;
- cancellation during multipart source reading prevents part upload and
  completion while still issuing one abort; and
- the abort cleanup context is detached from caller cancellation, preserves
  values, and carries a bounded deadline.

Final verification will include focused package tests, the full Go suite,
build, vet, scoped lint, race tests, pinned MinIO conformance, and the existing
Linux 386, Plan 9, and Windows portability checks.
