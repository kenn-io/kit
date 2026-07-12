# Windows Trusted Directory Owners

## Problem

Windows can assign ownership of a directory created by an elevated process to
the built-in Administrators group. A later non-elevated process may have full
control through a protected, trusted-principal-only DACL, but
`safefileio.EnsurePrivateDir` and `safefileio.ValidatePrivateDir` reject the
directory because their owner policy accepts only the current token user and
token owner.

This is inconsistent with the existing DACL policy, which already trusts the
current token user, token owner, LocalSystem, and built-in Administrators.

## Design

Keep the change limited to private directories on Windows. Directory ownership
is valid when it belongs to one of the same trusted principals allowed by the
directory DACL policy:

- current token user;
- current token owner;
- LocalSystem; or
- built-in Administrators.

Centralize this list in a small helper used by directory-owner verification so
the ownership and DACL policies cannot drift. Do not change current-user file
ownership checks, Unix permissions, reparse-point rejection, or the requirement
for a protected trusted-principal-only DACL.

## Error Handling

Failure to construct either well-known SID remains an error. Directories owned
by any principal outside the trusted set continue to fail closed with an
ownership error. Broad or inherited DACLs continue to be rejected independently
of acceptable ownership.

## Testing

Add table-driven Windows tests for the owner-policy helper. The tests must prove
that the current token user, token owner, LocalSystem, and built-in
Administrators are accepted, while an unrelated well-known SID and nil owner are
rejected. Existing integration tests continue to protect directory creation,
DACL validation, and file-owner behavior.
