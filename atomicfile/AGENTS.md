# atomicfile Instructions

## Scope

`atomicfile/` publishes whole files atomically: replace-style writes
(`WriteFile`, `Create`/`Commit`), create-if-absent writes (`WriteNew`,
`PublishNoReplace`, `RenameNoReplace`), and `SyncDir`. Keep it app-neutral:
no file formats, locking, or caller policy. Link inspection belongs to
`fslink/`; private-file creation belongs to `safefileio/`.

## Invariants

- Readers see the old or the new content, never a partial file: stage in a
  temporary file, fsync (unless `WithoutSync`), close, then rename.
- Never fall back to copying. Replacement uses `os.Rename` on Unix and
  `MoveFileEx(MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH)` on Windows;
  `RenameNoReplace` on Windows uses `MOVEFILE_WRITE_THROUGH` only. Never add
  `MOVEFILE_COPY_ALLOWED`: a cross-volume staging directory must fail.
- Never replace a symlink or junction at the target by default. The target
  is classified with `fslink.Classify` at `Create` and again just before the
  rename; a link fails with an error wrapping `fslink.ErrIsLink`.
  `WithFollowLink` resolves the chain (at most 40 hops) and replaces the
  file it names, leaving the links in place. Resolve each relative
  destination the way the system does: against the link's real parent
  (`filepath.EvalSymlinks` on Unix, `GetFinalPathNameByHandle` on Windows,
  since `EvalSymlinks` skips junctions), never by lexically joining `..`; a
  Windows destination rooted without a volume (`\dir`) uses that parent's
  volume. A directory target always fails.
- No-replace publication never checks-then-renames. `RenameNoReplace` uses
  `renameat2(RENAME_NOREPLACE)`, `renamex_np(RENAME_EXCL)`, or `MoveFileEx`
  without replace, and fails with `errors.ErrUnsupported` elsewhere. An
  existing entry, including a dangling link, fails with an error wrapping
  `fs.ErrExist`. `PublishNoReplace` tries a hard link first; keep the
  `linkFile` variable so tests can force the rename fallback.
- Never repair permissions of an existing file. The staged file gets its mode
  before any data is written (exact `WithPerm`, default 0600, or the existing
  regular target's bits with `WithPreserveMode`); `WithPrivate` stages through
  `safefileio.CreatePrivateTemp` and cannot be combined with those options.
- Remove the staging file on every failure path, and never create parent
  directories.
- `SyncDir` fsyncs a directory on Unix and is a documented no-op on Windows,
  which cannot fsync a directory handle. Durability there rests on the file
  fsync and `MOVEFILE_WRITE_THROUGH`.

## Tests

- Windows CI may lack the symlink privilege: symlink cases skip on
  `ERROR_PRIVILEGE_NOT_HELD`; junction cases (`fslink.CreateJunction`) never
  skip.
- Assert that directories hold only the expected entries so leaked staging
  files fail the test.
- `RenameNoReplace` success tests run only where it is supported
  (`darwin || linux || windows`); other Unix asserts `errors.ErrUnsupported`.
