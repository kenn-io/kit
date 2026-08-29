# Safe merge-driver fallback design

## Problem

Merge-request imports treat the fetched tree as untrusted. Kit therefore
replaces every configured custom merge driver that the tree can select. The
current replacement exits with status 1 without writing a result to Git's
`%A` file.

Git correctly records an unmerged index entry, but it trusts the custom driver
to populate `%A`. The working file consequently contains only the current
side, with no conflict markers or other-side content. A later `git add` can
hide the omitted changes.

## Goals

- Use Git's normal three-way text merge when a custom driver is disabled.
- Preserve diff3 conflict markers and all three inputs when overlap remains.
- Merge non-overlapping edits without reporting a conflict.
- Keep an imported tree from selecting a replacement executable through
  `PATH`.
- Preserve the existing filter, diff, hook, fsmonitor, submodule, and
  worktree-configuration isolation behavior.

## Non-goals

- Distinguishing drivers selected by trusted global attributes from drivers
  selected by repository-controlled attributes.
- Changing the public managed-worktree API.
- Replacing Git's built-in merge algorithm or interpreting file contents in
  Go.

## Design

Before preparing persistent untrusted-tree isolation, Kit resolves the same
`git` executable that its subprocess runner uses. It converts the result to an
absolute, cleaned path and fails the import if no executable can be resolved.

Kit builds the replacement custom-driver command from that trusted absolute
path. The command invokes:

```text
<absolute-git> merge-file --diff3 --marker-size=%L \
  -L "%X" -L "%S" -L "%Y" "%A" "%O" "%B"
```

The executable path is shell-quoted as data. On Windows, path separators are
normalized for the POSIX shell supplied by Git for Windows. The placeholders
remain available for Git to substitute when it runs the driver.

`git merge-file` overwrites `%A`, exits with status 0 for a clean merge, and
returns a nonzero status when conflicts remain. Git therefore keeps its normal
index state while the working file contains either a clean merged result or a
complete diff3 conflict.

The resolved command becomes part of the existing untrusted-tree isolation
state. Attribute-driver discovery remains responsible only for finding driver
names; it uses the prepared command when constructing worktree-scoped
`merge.<name>.driver` entries. No new public API or persistent file is added.

## Error handling

Failure to resolve an absolute Git executable stops the import before the
untrusted worktree is materialized. A later missing or unusable executable
causes Git to fail the merge command rather than run a same-named program found
through the worktree's `PATH`.

Existing import cleanup and rollback behavior remains unchanged.

## Tests

Behavioral tests create temporary repositories through the existing managed
worktree fixtures and exercise the persisted replacement after import:

1. Two non-overlapping edits merge cleanly and produce the combined file.
2. Overlapping edits leave an unmerged path whose working file contains diff3
   markers and the base, current, and other content.
3. A fake `git` executable placed before the trusted executable in `PATH` is
   not invoked during the merge.

Focused unit coverage verifies shell quoting for executable paths, including
spaces, quotes, and Windows separators where platform-specific handling is
needed.
