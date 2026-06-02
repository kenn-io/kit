# Safe File I/O Instructions

## Scope

`safefileio/` provides hardened file and directory helpers for local runtime
state that must belong to the current user. It is intentionally small. Keep
callers responsible for their own file formats and higher-level policy.

## Invariants

- Treat ambiguous paths as unsafe. Empty paths, symlinks, and non-regular files
  should fail before callers can write runtime state through them.
- Runtime directory validation should judge the directory entry the caller
  supplied, not a symlink target reached after traversal.
- `EnsurePrivateDir` may repair a real directory's permissions when that is safe.
  `ValidatePrivateDir` must report problems without repairing them.
- Keep permissions and ownership checks tied to current-user-only runtime state.
  On Windows, that means SID semantics rather than username string comparisons.
- If ownership or file type cannot be established, return an error.

## Tests

- Use testify for new or changed assertions.
- Keep Unix and Windows coverage in build-tagged test files.
- Permission tests may use fixed paths under the OS temp directory when
  `t.TempDir()` starts from permissions that hide the behavior under test.
- Clean up every path created outside `t.TempDir()`.
