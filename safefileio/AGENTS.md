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
- Keep file ownership checks tied to current-user-only runtime state. On
  Windows, private directories may also be owned by LocalSystem or built-in
  Administrators only when their protected DACL restricts access to the same
  trusted user, token-owner, system, and administrator principals. Use SID
  semantics rather than username string comparisons.
- If ownership or file type cannot be established, return an error.
- Private-file validation must inspect the same open handle used by the caller
  and must never mutate permissions. Existing broad access may already have
  produced handles that no in-place repair can revoke.
- On supported Unix platforms, require exact mode 0600 and no access ACL.
  Reject Linux network and user-space filesystems whose effective access policy
  cannot be verified through local mode and access-ACL operations.
- On Windows, require a protected DACL that grants access only to the current
  user and trusted administrative principals. Callers recovering a broad or
  inheritable file must create a private replacement rather than repair it in
  place.
- `CreatePrivateFile` (and `CreatePrivateTemp`, which retries it under
  random names) is the sanctioned way to make a private file. It creates the
  file exclusively without following links, private from the moment it exists
  (Unix: `O_EXCL|O_NOFOLLOW` with mode 0600; Windows: `CREATE_NEW` with a
  protected, non-inheritable DACL in the creation security attributes), then
  validates the new handle with the same private-file checks. It must never
  open or repair an existing file: anything already at the path, including a
  dangling symlink or junction, fails with an error wrapping `fs.ErrExist`.
- When post-creation validation fails, `CreatePrivateFile` fails closed and
  deletes only the file it created: Unix removes the path only while it still
  names the created inode; Windows marks the created handle for deletion.
  Platforms that cannot validate private files must fail creation the same way.

## Tests

- Use testify for new or changed assertions.
- Keep Unix and Windows coverage in build-tagged test files.
- Permission tests may use fixed paths under the OS temp directory when
  `t.TempDir()` starts from permissions that hide the behavior under test.
- Clean up every path created outside `t.TempDir()`.
- Windows link cases: symlink tests skip on `ERROR_PRIVILEGE_NOT_HELD`, so
  every link guarantee also needs a junction case (`fslink.CreateJunction`,
  from an external `safefileio_test` file) that never skips.
