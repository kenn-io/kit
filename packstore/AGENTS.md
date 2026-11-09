# packstore

## Loose publication

- Durable streaming duplicate detection precedes syncing the selected staging file
  when the caller could not check the hash and size before staging.
  A duplicate verifies and, for durable writes, syncs the retained canonical
  object; its discarded staging bytes need no sync. The early check holds the
  publication stripe so repair cannot replace or remove the object during
  verification, and releases it before staging sync. New writes and repairs
  still sync and close the selected file before taking the stripe for publication.
  New writes recheck for a concurrent publication there. All paths retain
  durable staging-entry cleanup and propagate its errors.

- Store-directory staging and the final shard can share a parent durability
  sync. Defer only the parent sync of an existing staging child: directory
  creation still syncs missing ancestors, and shard preparation must sync the
  root before publication or repair recovery. A concurrent creator does not
  establish durability for our write: recheck a newly created leaf and sync
  its parent even if our Mkdir finds it already exists. Staging at the root
  itself requires its own parent sync.
- Reused compression encoders must start independent streams. Return an encoder
  to the pool only after a successful close, and detach its destination before
  pooling so idle encoders retain no staging file handles.
- New loose content publishes with `atomicfile.PublishNoReplace`. Publication
  must never replace or copy over an existing canonical name: a lost race is
  resolved by verifying the existing object, not by overwriting it.
- `PublishNoReplace` may either leave the staging file in place (hard link) or
  consume it (no-replace rename). Staging cleanup must treat a missing staging
  file as success.
- Repair publication deliberately replaces: Unix uses `atomicfile.Replace`
  (`os.Rename` there), Windows uses `ReplaceFileW` so readers holding the old
  file keep a stable handle. Do not route Windows repair through `atomicfile`
  replace helpers without preserving those handle semantics and the
  reconcile/backup recovery paths.
- Repair recovery and the Windows absent-target path use
  `atomicfile.RenameNoReplace` behind package-level seams
  (`linkLooseRepairRecoveryFile`, `renameLooseRepairRecoveryFile`,
  `linkLooseRepairFileWindows`) that tests use to force each branch.
- Restore may replace damaged loose content after verifying the backup bytes,
  and removes damaged alternates. Valid alternate encodings must remain until
  the restored catalog is durable: the old catalog may still reference them.
  `backup.Restore` removes these valid alternates after its catalog sync; direct
  `RestoreLoose` callers own that cleanup.

## Background verification

- Existing-entry checks read packed content only for live entries with a canonical loose candidate; the preflight must stay at least as permissive as `findAnyValidCanonicalLoose`.
- A positive `PackOptions.MaxVerifyBytes` bounds existing packed-entry checks,
  continues in physical order across successful calls on the same maintainer,
  and leaves packing and required deletion checks active. Zero examines every
  indexed entry, reading packed bytes only as described above.
  Capture the last indexed entry at the start of each sweep
  so continued ingestion cannot postpone revisiting older entries. A failed or
  canceled pass keeps the entries it examined, so a deadline that keeps
  expiring cannot pin the sweep to one window. A repair found but not
  committed, through failure or a packing budget, moves the resume point back
  to just before that repair so it is found again; never advance past an
  uncommitted repair. A new maintainer starts a fresh sweep.

## Pack growth

- `PackOptions.MinSealBytes` and `MaxLooseAge` hold loose members so frequent
  passes do not seal one small pack each. They never hold repairs, and a
  candidate with no readable path counts as ready so packing reports it.
- `RepackSelection.MergeBelowStored` is opt-in: existing callers scope repack
  to chosen sources and count `PacksSelected`. Merges go through
  `CommitRepack` with every source of a group, and admit whole groups against
  `MaxBytes` so outputs stay near the target size.
- Orphan reconciliation lists pack records once per pass. Pack records change
  only under the maintenance lease or during a restore that already excludes
  maintenance.

## Orphan removal

- `Pack` removes unreferenced canonical loose files only at its end, after
  verification and packing. `SweepOrphans` runs that removal alone so a caller
  with a short time budget can reclaim space before `Pack`. Both hold the
  maintenance lease, because a write publishes its file before the catalog
  references it, and both remove nothing when the reference inventory is
  incomplete. An orphan without removal permission is counted as deferred and
  skipped, so it cannot block reclaiming the orphans after it.
