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
Windows executable loader. Existing untrusted-tree imports require Git 2.42.0
or newer on non-Windows platforms and Git for Windows 2.53.0.windows.3 or
newer. Before Git 2.42, every positive custom merge-driver status is treated as
an ordinary conflict, so a driver cannot distinguish a per-file conflict from
an operation error. Fixed labels themselves add no new version floor.

Kit moves the existing POSIX shell single-quote helper into a shared internal
Git package. Both the credential helper and managed-worktree isolation use it,
so executable paths remain shell data without adding a public API or
duplicating shell quoting. Merge-driver configuration has an earlier template
layer: Git expands percent placeholders before it invokes the shell. The merge
driver builder therefore doubles every literal `%` in embedded executable and
managed-directory paths before passing those paths to the POSIX shell-quote
helper. This percent escaping stays local to merge-driver construction because
it is a Git-template rule, not generic shell quoting.

Kit builds a shell function around the trusted absolute path. In expanded form,
the function has these semantics:

```sh
f() {
  [ -x '<absolute-git>' ] || return 129
  # Convert the three merge inputs to absolute paths before changing Git's cwd.
  # Classify current/base and base/other with the pinned Git executable:
  #   GIT_ATTR_NOSYSTEM=1 GIT_CEILING_DIRECTORIES='<managed-parent>' \
  #     '<absolute-git>' -c core.attributesFile= -C '<managed-empty-dir>' \
  #     diff --no-index --numstat --no-ext-diff --no-textconv -- "$left" "$right"
  # Return 1 before merge-file for binary content and 129 on classifier errors.
  '<absolute-git>' merge-file --diff3 --marker-size="$1" \
    -L current -L base -L other "$2" "$3" "$4"
  return $?
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
Fixed labels add no version requirement beyond the non-Windows Git 2.42.0 and
Git for Windows 2.53.0.windows.3 minimums, with no version-dependent fallback.

Before invoking `merge-file`, the wrapper asks the pinned Git executable to
classify the inputs with `diff --no-index --numstat`. It converts the temporary
input names to absolute paths, runs Git from the managed empty hooks directory,
and sets the discovery ceiling to that directory's parent. System and global
attribute files are disabled, and repository discovery is prevented, so
worktree attributes cannot falsely force content to text or binary. The
`--no-ext-diff` and `--no-textconv` flags prevent configured diff and textconv
helpers from running. A binary numstat result returns status 1 before
`merge-file`; a classifier command error or malformed result returns status
129. The wrapper compares current to base and base to other so an unchanged
pair cannot hide binary content in the remaining side.

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

Binary classification returns status 1 without invoking `merge-file`. Git
keeps the current bytes, marks that path conflicted, and continues processing
other paths. Binary files receive neither text markers nor `merge-file`'s
temporary-file diagnostic.

Every `merge-file` status passes through unchanged. In particular, status 255
is not assumed to mean binary: `merge-file` also uses it for input, output,
read, write, and close failures. With the Git 2.42 floor, that status aborts the
operation instead of becoming an ours-only conflict. A signal death such as
status 139 likewise remains an operation error, while statuses 1 through 127
remain valid text conflict counts. The executable guard covers a stable
missing or non-executable path, but not a same-user replacement race between
the check and invocation.

Existing import cleanup and rollback behavior remains unchanged.

## Tests

Behavioral tests create temporary repositories through the existing managed
worktree fixtures and exercise the persisted replacement after import:

1. Two non-overlapping edits merge cleanly and produce the combined file.
2. Overlapping edits leave an unmerged path whose working file contains diff3
   markers with the fixed labels and the base, current, and other content.
3. Adversarial branch and commit labels remain inert during a conflicted merge.
4. A resolved Git path containing literal `%Y` cannot expand an untrusted
   branch label before shell quoting or execute its payload.
5. A missing persisted executable aborts the merge and leaves a clean tree.
6. Binary content produces an ordinary per-file conflict alongside a text
   conflict, even when worktree attributes try to force the opposite content
   classifications.
7. A simulated non-binary `merge-file` status 255 aborts the operation without
   leaving a current-only conflict.
8. Git 2.41 is rejected and Git 2.42 is accepted on non-Windows platforms.
9. The existing PATH-hijack fixture is extended so a fake `git` executable
   placed before the trusted executable is not invoked during a merge.

Focused unit coverage moves with the shared single-quote helper and covers
executable paths containing spaces and single quotes. Existing assertions for
the persisted merge-driver value compare against the computed command.
Behavioral tests 1 through 6 run on Unix and Windows, including the emitted
Windows path form. The merge-file error simulator and PATH-hijack fixture are
the only POSIX-only tests.
