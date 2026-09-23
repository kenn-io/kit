# fsname Instructions

## Scope

`fsname/` decides whether a single file or directory name is portable across
modern operating systems and file systems, and derives a portable name from an
outside string. It is lexical: it never touches the file system. Link and
open safety belong to `fslink/`.

## Invariants

- `Check` rejects; it never renames. Use it where a caller supplies a name
  kit will create or look up. `Clean` and `Join` rewrite; use them only where
  the caller asks kit to derive a name from outside data.
- `Check(name)` succeeds exactly when `Clean(name) == name` and name is a
  single element other than "." or "..". Keep the two in lockstep.
- Do not use these functions on full host paths or on names restored from a
  backup: those must keep their exact names and use `filepath.IsLocal`.
- The rules come from `github.com/spf13/pathologize`. Names it misses
  (`CONIN$`, `CONOUT$`) are added here; remove an addition once pathologize
  covers it instead of keeping both.
- `Join` output must always satisfy `filepath.IsLocal`.
- `CheckPath` rejects Windows path forms that read differently from how they
  resolve: the device namespace (`\\.\`, `\??\`), drive-relative (`C:x`) and
  driveless rooted (`\x`) paths always; network shares, `\\?\` long paths and
  8.3 short names unless the caller opts in. Defaults refuse, so a caller
  who never considered a form does not accept it silently.
- Keep the Windows rules in `checkPath(path, windows, cfg)` so they are
  tested on every platform, not only in Windows CI.

## Tests

- Use testify and table tests. Cover every reserved-name addition with and
  without an extension and in mixed case, and every CheckPath form both
  refused and, where an option exists, allowed.
