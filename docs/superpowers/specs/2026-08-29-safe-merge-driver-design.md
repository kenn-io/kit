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
- Preserve Git's ordinary per-file conflict behavior for binary content.
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
On Windows, Kit emits the drive-qualified path with forward slashes before
shell quoting it. Git for Windows runs custom drivers through its POSIX shell,
and the forward-slash form works for both the shell's executable check and the
Windows executable loader.

Kit moves the existing POSIX shell single-quote helper into a shared internal
Git package. Both the credential helper and managed-worktree isolation use it,
so executable paths remain data without adding a public API or duplicating
shell quoting.

Kit builds a shell function around the trusted absolute path. In expanded form,
the function has these semantics:

```sh
f() {
  [ -x '<absolute-git>' ] || return 129
  '<absolute-git>' merge-file --diff3 --marker-size="$1" \
    -L current -L base -L other "$2" "$3" "$4"
  status=$?
  if [ "$status" -eq 255 ]; then
    return 1
  fi
  return "$status"
}
f %L "%A" "%O" "%B"
```

The displayed single quotes around `<absolute-git>` represent the shared
helper's output, including its handling of an embedded single quote.

Git inserts `%A`, `%O`, and `%B` without shell quoting, so the driver command
must keep the double quotes around those placeholders. Git 2.44 and newer
already insert `%S`, `%X`, and `%Y` as shell-single-quoted strings. Placing those
label placeholders inside another pair of double quotes would make command
substitutions in a branch name or commit subject executable. The replacement
therefore uses fixed labels and does not interpolate `%S`, `%X`, or `%Y` at all.
Fixed labels also keep the existing Git 2.39.1 minimum; no version-dependent
fallback or higher version floor is needed.

`git merge-file` overwrites `%A`, exits with status 0 for a clean merge, and
returns the conflict count, capped at 127, when text conflicts remain. The
wrapper passes those statuses through. Git therefore keeps its normal index
state while a text working file contains either a clean merged result or a
complete diff3 conflict.

The resolved command becomes part of the existing untrusted-tree isolation
state. Attribute-driver discovery uses the prepared command when constructing
worktree-scoped `merge.<name>.driver` entries. The command-scope configuration
check continues to classify driver-shaped keys without constructing a merge
command, so that earlier call site does not need a resolved executable path.
No new public API or persistent file is added.

## Error handling

Failure to resolve an absolute Git executable stops the import before the
untrusted worktree is materialized. Before every later merge-driver invocation,
the shell function checks that the persisted path is still executable. A moved
or removed executable returns status 129 before `merge-file` starts. Git treats
that status as a driver failure and aborts the operation instead of recording
an ours-only conflict.

`git merge-file` rejects binary content with status 255. After the executable
guard succeeds, the wrapper maps that exact status to status 1.
This preserves the previous and built-in binary-driver behavior: Git keeps the
current bytes, marks that path conflicted, and continues processing other
paths. Binary files do not receive text conflict markers. The guard runs first,
so an unavailable persisted executable is not converted into an ordinary
conflict. `merge-file` still writes its binary-file diagnostic to standard
error, including Git's temporary filename.

All other statuses pass through unchanged. In particular, a signal death such
as status 139 remains a driver failure instead of becoming an ours-only
conflict. Statuses 126 and 127 also remain unchanged because `merge-file` can
legitimately return those conflict counts. The executable guard covers a stable
missing or non-executable path, but not a same-user replacement race between the
check and invocation. Git also treats `merge-file`'s own status 128 as an
ordinary conflict; its status contract provides no distinct value that the
wrapper can safely reinterpret.

Existing import cleanup and rollback behavior remains unchanged.

## Tests

Behavioral tests create temporary repositories through the existing managed
worktree fixtures and exercise the persisted replacement after import:

1. Two non-overlapping edits merge cleanly and produce the combined file.
2. Overlapping edits leave an unmerged path whose working file contains diff3
   markers with the fixed labels and the base, current, and other content.
3. Adversarial branch and commit labels remain inert during a conflicted merge.
4. A missing persisted executable aborts the merge and leaves a clean tree.
5. Binary content produces an ordinary per-file conflict instead of aborting
   the whole merge.
6. The existing PATH-hijack fixture is extended so a fake `git` executable
   placed before the trusted executable is not invoked during a merge.

Focused unit coverage moves with the shared single-quote helper and covers
executable paths containing spaces and single quotes. Existing assertions for
the persisted merge-driver value compare against the computed command.
Behavioral tests 1 through 5 run on Unix and Windows, including the emitted
Windows path form. Only the POSIX PATH-hijack fixture in test 6 skips Windows.
