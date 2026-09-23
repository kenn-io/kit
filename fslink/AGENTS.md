# fslink Instructions

## Scope

`fslink/` classifies, reads, and creates symlinks and Windows junctions, and
opens paths without following them. Keep it app-neutral: no caller policy,
file formats, or ownership rules belong here (see `safefileio/` for those).

## Invariants

- On Windows, "is a link" means a name-surrogate reparse tag
  (`tag & 0x20000000 != 0`), read from a handle opened with
  `FILE_FLAG_OPEN_REPARSE_POINT`. Do not treat every reparse point as a link:
  cloud-file placeholders, dedup, and WOF entries hold data in place. Do not
  rely on `os.Lstat` mode bits: since Go 1.23 junctions and cloud placeholders
  both report `ModeIrregular`.
- `IO_REPARSE_TAG_MOUNT_POINT` is `Junction` unless its substitute name is a
  `\??\Volume{...}` path, which is `OtherLink`.
- `Classify`, `IsLink`, `Readlink`, `OpenFile`, `OpenRegular`, and `ReadFile`
  judge only the final path component; earlier components may be links.
- `OpenInRoot` and `OpenRootNoFollow` refuse a link in any component. Each
  component is inspected without following it, opened through the `*os.Root`,
  and compared by file identity with the inspected entry. On Windows the
  inspection is relative to the parent root's own handle; never reopen by
  absolute path.
- Never follow a link in the components these functions guard. A refused link
  returns an `*fs.PathError` wrapping `ErrIsLink`.
- Never truncate or create through a link: `O_TRUNC` is applied only after the
  opened handle is verified, and root-confined creation uses `O_EXCL`.
- `OpenRegular` must not block on FIFOs (Unix opens with `O_NONBLOCK`) and must
  reject anything but a regular file.
- `CreateJunction` must not require administrator rights or Developer Mode; it
  sets the mount-point reparse data itself. `LinkDir` falls back to a junction
  only on `ERROR_PRIVILEGE_NOT_HELD`.
- On Windows, `OpenFile`, `OpenRegular`, and `ReadFile` inspect the final
  component with `FILE_FLAG_OPEN_REPARSE_POINT`, but a non-link reparse point
  must be reopened without that flag (and matched by file identity) before it
  is read or truncated; the flag bypasses the filter driver that serves the
  data of placeholders, dedup, and WOF files.
- On Windows, `LinkDir` always creates a directory symlink
  (`SYMBOLIC_LINK_FLAG_DIRECTORY`), even for a missing target; `os.Symlink`
  would silently create a file symlink. Its junction fallback resolves a
  rooted target without a volume against the link's volume.
- On platforms other than Unix and Windows, `Classify`, `IsLink`, and
  `Readlink` use `os.Lstat`/`os.Readlink` (these platforms have no
  junctions); every open and link-creating function fails closed with
  `errors.ErrUnsupported`.
- The Unix no-follow errno set is per OS (`nofollow_*.go`): NetBSD reports
  `EFTYPE` where others report `ELOOP` or `EMLINK`.

## Tests

- Windows CI may lack the symlink privilege: symlink cases skip on
  `ERROR_PRIVILEGE_NOT_HELD`, junction cases never skip.
- Symlink fixtures use relative in-tree targets so `os.Root` would follow them;
  otherwise the any-component tests would pass on `os.Root`'s own escape check.
