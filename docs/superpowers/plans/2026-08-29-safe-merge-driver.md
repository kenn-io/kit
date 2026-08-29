# Safe Merge Driver Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use `superpowers:executing-plans` to implement this plan task-by-task.

**Goal:** Replace untrusted-tree custom merge drivers with a durable driver that performs ordinary three-way text merges, writes diff3 conflict markers, keeps binary conflicts local to the file, and fails the whole Git operation when the pinned Git executable cannot run.

**Architecture:** Resolve the same `git` executable used by `git/cmd.Runner` before materializing the untrusted tree, persist an absolute shell-quoted path in worktree config, and invoke `git merge-file` from a small shell wrapper. Keep attribute-driver key classification independent from command construction so command-scope checks do not require path resolution. Move the existing POSIX single-quote helper into `git/internal/shellquote` so both owning packages share the escaping rule.

**Tech Stack:** Go 1.26, Git 2.39.1+, `testify`, standard `os/exec`, repository lifecycle fixtures.

---

## Task 1: Share the existing shell-quoting helper

**Files:**
- Create: `git/internal/shellquote/shellquote.go`
- Create: `git/internal/shellquote/shellquote_test.go`
- Modify: `git/cmd/gitcmd.go:18-28,187-198`
- Modify: `git/cmd/gitcmd_test.go:569`

### Step 1: Write the failing helper test

Create a table test for the exact POSIX single-quote contract:

```go
package shellquote

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSingle(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, input, want string
	}{
		{name: "empty", want: "''"},
		{name: "spaces", input: "/opt/Git Tools/git", want: "'/opt/Git Tools/git'"},
		{name: "single quote", input: "/opt/Git's/git", want: "'/opt/Git'\\''s/git'"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, test.want, Single(test.input))
		})
	}
}
```

Run: `go test ./git/internal/shellquote`

Expected: FAIL because `Single` does not exist.

### Step 2: Implement and adopt the helper

```go
// Package shellquote quotes arguments embedded in Git shell command strings.
package shellquote

import "strings"

// Single returns value enclosed in POSIX shell single quotes.
func Single(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
```

Import it in `git/cmd/gitcmd.go`, replace `shellSingleQuote(path)` with `shellquote.Single(path)`, and delete the local helper. Make the same replacement in `gitcmd_test.go`. Retain the existing `strings` import because the package has other users.

### Step 3: Verify and commit the refactor

Run:

```sh
gofmt -w git/internal/shellquote/shellquote.go git/internal/shellquote/shellquote_test.go git/cmd/gitcmd.go git/cmd/gitcmd_test.go
go test ./git/internal/shellquote ./git/cmd
```

Commit:

```sh
git add git/internal/shellquote git/cmd/gitcmd.go git/cmd/gitcmd_test.go
git commit -m "refactor(git): share shell quoting helper"
```

## Task 2: Add end-to-end merge-driver regression tests

**Files:**
- Create: `git/managed/untrusted_tree_merge_test.go`
- Modify: `git/managed/lifecycle_mr_test.go:440-725,619-675,803-872`

### Step 1: Add a reusable imported-worktree fixture

Build real repositories with `initOriginAndClone`, commit `.gitattributes` containing `payload merge=owned`, create `current` and `other` commits from a shared base, fetch `other` into the clone, and call `CreateWorktreeFromMergeRequest` for `current`. Return the imported worktree path and the other ref. Keep all paths under `t.TempDir`; do not read or alter user Git configuration.

The helper should accept base/current/other bytes and optional branch/subject strings so every test exercises the persisted worktree driver through ordinary `git merge` or `git rebase`, rather than invoking the wrapper directly:

```go
type mergeDriverFixture struct {
	worktree string
	otherRef string
}

func newMergeDriverFixture(
	t *testing.T, base, current, other []byte,
	currentSubject, otherBranch string,
) mergeDriverFixture
```

Configure the trusted clone with `merge.owned.driver=false` before import. The returned worktree must therefore succeed only if Kit replaces that configured driver.

### Step 2: Write the five cross-platform behavior tests

Add these tests without any Windows skip:

1. `TestUntrustedTreeMergeDriverMergesNonOverlappingText` — merge edits to different lines; assert exit 0, both edits in `payload`, and no unmerged entries.
2. `TestUntrustedTreeMergeDriverWritesDiff3ConflictMarkers` — overlap one line; assert ordinary conflict, `UU payload`, and exact marker labels `<<<<<<< current`, `||||||| base`, `=======`, `>>>>>>> other` with all three bodies.
3. `TestUntrustedTreeMergeDriverDoesNotEvaluateGitLabels` — use a branch name and rebase commit subject containing `$()` and backticks that would create relative marker files; assert neither marker exists and conflict markers still use only the fixed labels.
4. `TestUntrustedTreeMergeDriverFailsWhenResolvedGitDisappears` — copy the resolved executable to a temporary executable path, replace only the fixture worktree's `merge.owned.driver` value with the command constructed for that copy, remove it before merge, and assert Git aborts with a clean worktree rather than recording `UU` with current-only contents.
5. `TestUntrustedTreeMergeDriverKeepsBinaryConflictLocal` — mark both `binary.dat` and `payload` with the same driver; conflict both in one merge; assert both are `UU`, binary bytes remain current without text markers, and `payload` contains diff3 markers. Accept and document the `Cannot merge binary files` stderr line naming Git's temporary file.

For the hostile-label test, use relative marker names valid under both Git for Windows' shell and POSIX shells; do not embed a platform-specific absolute path in a ref name. For the missing-executable test, use `git config --worktree merge.owned.driver <computed-command>` after import. This tests the persisted-command failure contract without adding a production seam; ordinary production preparation still calls `exec.LookPath` exactly once.

### Step 3: Extend the existing POSIX PATH-hijack test

Keep the current `runtime.GOOS == "windows"` skip only on `TestCreateWorktreeFromMergeRequestDoesNotPATHSearchDriverHelpers`. Add `merge=owned` to its attributes, configure `merge.owned.driver=false`, create a conflicting branch, run the later merge with the imported tree first in `PATH`, and preserve `assert.NoFileExists(marker)`. This verifies the wrapper invokes the absolute Git path instead of a tree-controlled `git` or `sh` helper.

### Step 4: Run the new tests and confirm RED

Run:

```sh
go test ./git/managed -run 'Test(UntrustedTreeMergeDriver|CreateWorktreeFromMergeRequestDoesNotPATHSearchDriverHelpers)' -count=1
```

Expected: FAIL. Non-overlapping text remains current-only, overlapping text lacks conflict markers, and the computed-command test seams do not yet exist.

Do not commit these failing tests separately.

## Task 3: Build and persist the safe merge driver

**Files:**
- Modify: `git/managed/untrusted_tree.go:1-40,144-168,217-227,261-300,526-580`
- Modify: `git/managed/lifecycle_mr_test.go:522,672,870,1057`
- Test: `git/managed/untrusted_tree_merge_test.go`

### Step 1: Resolve and normalize Git before materialization

Add `os/exec` and `git/internal/shellquote` imports. Resolve with the same process `PATH` semantics as `gitcmd.Runner`:

```go
func resolveMergeDriverGitPath() (string, error) {
	path, err := exec.LookPath("git")
	if err != nil {
		return "", fmt.Errorf("resolve Git executable for safe merge driver: %w", err)
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make Git executable path absolute: %w", err)
	}
	path = filepath.Clean(path)
	if runtime.GOOS == "windows" {
		path = filepath.ToSlash(path)
	}
	return path, nil
}
```

Call this from `prepareUntrustedTreeIsolation` after command-scope validation and before creating or materializing the worktree. Store the generated command on `untrustedTreeIsolation`:

```go
type untrustedTreeIsolation struct {
	runner             gitcmd.Runner
	config             []gitcmd.Config
	mergeDriverCommand string
}
```

### Step 2: Construct the exact wrapper

Replace the constant with a builder. The generated command must be semantically equivalent to:

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

Build it as one line for Git config:

```go
func safeMergeDriverCommand(gitPath string) string {
	git := shellquote.Single(gitPath)
	return `f() { [ -x ` + git + ` ] || return 129; ` + git +
		` merge-file --diff3 --marker-size="$1" -L current -L base -L other "$2" "$3" "$4"; ` +
		`status=$?; if [ "$status" -eq 255 ]; then return 1; fi; ` +
		`return "$status"; }; f %L "%A" "%O" "%B"`
}
```

Do not use `%S`, `%X`, or `%Y`: Git already shell-quotes those placeholders, and outer double quotes would turn hostile label content into executable shell syntax. Preserve the double quotes around `%A`, `%O`, and `%B`, which Git substitutes without shell quoting.

Map only status 255 to conflict. Do not use `-gt 128`, and do not remap 126 or 127: signal deaths must remain Git operation errors, while `merge-file` can legitimately report text conflict counts through 127. The executable guard returns 129 before invocation. An internal `merge-file` `die()` can still exit 128 and be treated by Git as a conflict; Git's custom-driver protocol provides no distinguishable alternative.

### Step 3: Separate classification from construction

Extract attribute-driver discovery from config creation:

```go
type attributeDrivers struct {
	filters map[string]struct{}
	diffs   map[string]struct{}
	merges  map[string]struct{}
}

func configuredAttributeDrivers(keys []string) attributeDrivers

func (drivers attributeDrivers) configured() bool {
	return len(drivers.filters)+len(drivers.diffs)+len(drivers.merges) != 0
}

func neutralizeAttributeDrivers(
	drivers attributeDrivers, mergeDriverCommand string,
) []gitcmd.Config
```

Use `configuredAttributeDrivers([]string{key}).configured()` in `isolationSensitiveConfigKey`, where no path is available. In `completeUntrustedTreeIsolation`, discover once and pass `isolation.mergeDriverCommand` to config construction.

### Step 4: Update computed-command assertions

Add one test helper used by all three existing config assertions:

```go
func expectedSafeMergeDriverCommand(t *testing.T) string {
	t.Helper()
	path, err := resolveMergeDriverGitPath()
	require.NoError(t, err)
	return safeMergeDriverCommand(path)
}
```

Replace constant equality checks at the existing isolation, case-distinct-driver, and selected-config tests. Update the direct `neutralizeAttributeDrivers` classification test for the split API.

### Step 5: Run focused tests and commit

Run:

```sh
gofmt -w git/managed/untrusted_tree.go git/managed/untrusted_tree_merge_test.go git/managed/lifecycle_mr_test.go
go test ./git/managed -run 'Test(UntrustedTreeMergeDriver|CreateWorktreeFromMergeRequest(IsolatesUntrustedTreeGitPrograms|NeutralizesCaseDistinctAttributeDrivers|DoesNotPATHSearchDriverHelpers|InspectsSelectedConfigFiles))' -count=1
go test ./git/managed -count=1
```

Expected: PASS. Inspect the focused test output to ensure binary rejection is represented as an ordinary conflict while missing executables and signal deaths are operation errors.

Commit:

```sh
git add git/managed/untrusted_tree.go git/managed/untrusted_tree_merge_test.go git/managed/lifecycle_mr_test.go
git commit -m "fix(git): preserve merge conflict contents"
```

## Task 4: Document the durable boundary and verify the repository

**Files:**
- Modify: `git/AGENTS.md`

### Step 1: Record the maintained invariant

In the untrusted merge-request guidance, state that replacement merge drivers must:

- invoke the resolved Git executable without a worktree `PATH` lookup;
- write clean text merges and diff3 markers for text conflicts;
- keep binary rejection as an ordinary per-file conflict;
- surface missing executables and process crashes as whole-operation errors;
- behave explicitly on Unix and Git for Windows.

Do not document the wrapper string itself; document the behavior future implementations must preserve.

### Step 2: Run fresh full verification

Run:

```sh
gofmt -w git/cmd/gitcmd.go git/cmd/gitcmd_test.go git/internal/shellquote/*.go git/managed/untrusted_tree.go git/managed/*_test.go
go test ./git/... -count=1
go vet ./...
go test ./... -count=1
make lint
git diff --check
git status --short
```

If `make lint` creates the ignored `custom-gcl` binary, leave it untracked/ignored and do not add it. Review `git diff origin/main...HEAD` for unrelated changes and private data before any push.

### Step 3: Commit documentation if changed

```sh
git add git/AGENTS.md
git commit -m "docs(git): define safe merge driver behavior"
```

Skip this commit if the existing text already expresses the complete invariant and no documentation edit is necessary.

## Task 5: Push and open the Kit pull request

### Step 1: Perform pre-push checks

Follow `kenn:commit`, `kenn:scrub-private-data`, `superpowers:verification-before-completion`, and `kenn:commit-push-pr`. Confirm every accepted repository change is committed and the branch contains no credentials, private paths, private downstream names, or generated scratch files.

### Step 2: Push the branch

```sh
git push -u origin fix/merge-driver-conflict-markers
```

### Step 3: Open the PR

Draft the body with `kenn:pr-desc`, describing the current result and reviewer-relevant tradeoffs. Do not include a routine `Validation` section and do not comment on the issue or poll CI.

```sh
gh pr create \
  --base main \
  --head fix/merge-driver-conflict-markers \
  --title "fix(git): preserve merge conflict contents" \
  --body-file <temporary-private-body-file>
```

The body should explain that imported worktrees now retain normal clean merges and diff3 text conflicts, binary conflicts remain per-file, and a moved/crashed pinned Git executable aborts instead of silently leaving current-only content. Link the originating public issue only if it is appropriate for the Kit repository's public context.
