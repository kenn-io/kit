# packstore

## Loose publication

- Store-directory staging and the final shard can share a parent durability
  sync. Defer only the parent sync of an existing staging child: directory
  creation still syncs missing ancestors, and shard preparation or durable
  dedup verification must sync the root before publication or repair recovery.
  Staging at the root itself requires its own parent sync.
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
