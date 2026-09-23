# atomicfile Instructions

## Scope

`atomicfile/` publishes whole files atomically: replace-style writes
(`WriteFile`, `Create`/`Commit`), create-if-absent writes (`WriteNew`,
`PublishNoReplace`, `RenameNoReplace`), and `SyncDir`. Keep it app-neutral:
no file formats, locking, or caller policy. Link inspection belongs to
`fslink/`; private-file creation belongs to `safefileio/`.

## Invariants

- On Windows, every Win32 call that takes a caller's path converts it with
  `internal/winpath.UTF16Ptr`, which adds the `\\?\` prefix to long paths as
  the os package does. Without it a path that `os.OpenFile` accepts past
  MAX_PATH fails here.
- Readers see the old or the new content, never a partial file: stage in a
  temporary file, fsync (unless `WithoutSync`), close, then rename.
- Never fall back to copying. Replacement uses `os.Rename` on Unix and
  `MoveFileEx(MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH)` on Windows;
  `RenameNoReplace` on Windows uses `MOVEFILE_WRITE_THROUGH` only. Never add
  `MOVEFILE_COPY_ALLOWED`: a cross-volume staging directory must fail.
- Trust boundary: the target and staging directories are assumed not to be
  modifiable by untrusted users. Do not add defenses against an attacker who
  can rename entries there; they could replace the published file directly.
- By default, refuse a symlink or junction at the target when checked: the
  target is classified with `fslink.Classify` at `Create` and again just
  before the rename, and a link fails with an error wrapping
  `fslink.ErrIsLink`. This is a check, not an atomic guarantee: a link
  installed between the check and the rename is replaced (the rename never
  follows it). A directory target always fails.
- `WithFollowLink` resolves the chain (at most 40 hops) and replaces the file
  it names, leaving the links in place. `Commit` re-resolves the chain and
  fails, discarding the staging file, if it now leads elsewhere. Resolve
  destinations the way the system does. On Unix, keep the destination
  unclean (`realParent + "/" + dest`), split off the last element, and
  `filepath.EvalSymlinks` the rest, so `..` applies after each link resolves;
  never `filepath.Join` before resolving. On Windows, join the destination
  to the link's real parent (`GetFinalPathNameByHandle`, since
  `EvalSymlinks` skips junctions): Win32 normalization applies `..`
  lexically before reparse points resolve, so lexical joining is correct
  there. A Windows destination rooted without a volume (`\dir`) uses that
  parent's volume.
- No-replace publication never checks-then-renames. `RenameNoReplace` uses
  `renameat2(RENAME_NOREPLACE)`, `renamex_np(RENAME_EXCL)`, or `MoveFileEx`
  without replace, and fails with `errors.ErrUnsupported` elsewhere. An
  existing entry, including a dangling link, fails with an error wrapping
  `fs.ErrExist`. `PublishNoReplace` tries a hard link first; keep the
  `linkFile` variable so tests can force the rename fallback. `WriteNew`
  publishes through `publishNew`: `PublishNoReplace` on Unix, but
  `RenameNoReplace` on Windows, because `CreateHardLink` has no
  write-through and `SyncDir` cannot make the new name durable there.
- Never repair permissions of an existing file. The staged file gets its mode
  before any data is written (exact `WithPerm`, default 0600, or the existing
  regular target's bits with `WithPreserveMode`); `WithPrivate` stages through
  `safefileio.CreatePrivateTemp` and cannot be combined with those options.
- Remove the staging file on every failure path, and never create parent
  directories.
- `SyncDir` fsyncs a directory on Unix and is a documented no-op on Windows,
  which cannot fsync a directory handle. Durability there rests on the file
  fsync and `MOVEFILE_WRITE_THROUGH`.
- After publication, sync the target's directory and, when `WithStagingDir`
  names a different directory (by cleaned absolute path), the staging
  directory too. Call directory sync through the `syncDir` variable so tests
  can inject failures.
- Every failure after the target became visible wraps `ErrPublished` and its
  cause, so callers can tell "published" from "not published". A directory
  sync failure also wraps `ErrNotDurable` (which wraps `ErrPublished`); a
  cleanup failure alone, such as removing `WriteNew`'s leftover staging name
  after the syncs succeeded, does not, because the target is durable.
- Resolve the caller's path before taking its directory: `canonicalPath`
  resolves the parent (`EvalSymlinks` on the unclean parent on Unix, `Abs` on
  Windows) and makes it absolute, so `a/hop/../x` stages and publishes in the
  directory the kernel reaches and the `WithFollowLink` recheck compares
  equal spellings equally. Compare directories by identity (`os.SameFile`),
  not by string.

## Tests

- Windows CI may lack the symlink privilege: symlink cases skip on
  `ERROR_PRIVILEGE_NOT_HELD`; junction cases (`fslink.CreateJunction`) never
  skip.
- Assert that directories hold only the expected entries so leaked staging
  files fail the test.
- `RenameNoReplace` success tests run only where it is supported
  (`darwin || linux || windows`); other Unix asserts `errors.ErrUnsupported`.
