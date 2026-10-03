---
title: "packstore publication invariants"
description: "Publication and recovery invariants for loose packed-content objects, compression reuse, and cross-platform repair."
last_edited: 2026-10-02
---
# packstore

## Loose publication

- Reused compression encoders must start independent streams. Return an encoder
  to the pool only after a successful close, and detach its destination before
  pooling so idle encoders retain no staging file handles.
- New loose content publishes with `atomicfile.PublishNoReplace`. Publication
  must never replace or copy over an existing canonical name: a lost race is
  resolved by verifying the existing object, not by overwriting it.
- `PublishNoReplace` may either leave the staging file in place (hard link) or
  consume it (no-replace rename). Staging cleanup must treat a missing staging
  file as success.
- Repair publication deliberately replaces: Unix uses `os.Rename`, Windows uses
  `ReplaceFileW` so readers holding the old file keep a stable handle. Do not
  route repair through `atomicfile` replace helpers without preserving those
  handle semantics and the reconcile/backup recovery paths.
- Repair recovery and the Windows absent-target path use
  `atomicfile.RenameNoReplace` behind package-level seams
  (`linkLooseRepairRecoveryFile`, `renameLooseRepairRecoveryFile`,
  `linkLooseRepairFileWindows`) that tests use to force each branch.
