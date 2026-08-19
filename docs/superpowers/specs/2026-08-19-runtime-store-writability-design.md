# Runtime Store Writability Design

## Problem

Custom daemon auto-start callers can discover runtime records through
`RuntimeStore` without using `Manager.Ensure`. If the runtime directory is
readable but not writable, such a caller can start a detached child that exits
before publishing its runtime record. The caller then waits for readiness and
reports a generic timeout instead of the filesystem error.

Callers currently have no explicit, application-neutral way to preflight the
runtime store. Reimplementing the check downstream duplicates the store's path
and private-directory rules.

## Scope

Add this public method to `daemon.RuntimeStore`:

```go
func (s RuntimeStore) CheckWritable() error
```

The method validates the store configuration, prepares the private runtime
directory, creates and closes a uniquely named probe file in that directory,
and removes the probe. A successful call leaves no file behind.

This is an advisory check of the calling process's current filesystem access.
It does not detect a particular sandbox, guarantee that access will remain
available, or prove that a child process has identical permissions.

## Error Contract

Each failed operation returns an error with runtime-store context while
preserving the underlying error for `errors.Is`. In particular, callers can
detect `os.ErrPermission` and add application-specific recovery guidance.

Cleanup is attempted after every successful create. If close succeeds but
removal fails, `CheckWritable` returns the removal error instead of reporting a
clean preflight while leaving the probe behind.

## Boundaries

`StartDetached` remains a general process launcher and does not gain runtime
store configuration. `Manager.Ensure` keeps its existing start-lock behavior.
Callers that need the full managed lifecycle should continue to use `Manager`;
custom lifecycle callers can invoke `CheckWritable` immediately before spawn.

No application names, sandbox markers, retry policy, or user-facing guidance
belong in Kit.

## Tests

Package tests exercise the public method against real temporary filesystem
state:

- a valid runtime store succeeds and leaves its directory empty;
- a store whose directory cannot be prepared returns the filesystem failure;
- the returned error retains its underlying identity where the operating
  system supplies a comparable sentinel.

The implementation test is written and observed failing before production code
is added.

## Downstream Adoption

A downstream caller replaces its local probe with `RuntimeStore.CheckWritable`
after updating to the Kit release that contains the method. It retains its own
error wording and integration coverage.
