# Runtime Store Writability Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add an application-neutral `RuntimeStore.CheckWritable` preflight that custom daemon lifecycle callers can use before spawning a child.

**Architecture:** The public method stays on `RuntimeStore`, reuses its private-directory and prefix validation, and proves current write access with a uniquely named temporary file. The probe name is outside runtime-record and lock namespaces, and callers retain responsibility for product-specific error messages and lifecycle policy.

**Tech Stack:** Go standard library, `github.com/stretchr/testify`, Kit's existing `daemon` and `safefileio` packages.

## Global Constraints

- Keep the API application-neutral; do not add application names, sandbox markers, retry policy, or user-facing recovery guidance.
- Preserve underlying filesystem errors for `errors.Is`, including `os.ErrPermission` where the operating system supplies it.
- Treat the result as advisory current-process evidence, not a guarantee of future or child-process access.
- Keep probe names outside `<prefix>.<pid>.json`, `<prefix>.lock`, and `<prefix>.listen.lock`.
- Attempt cleanup after every successful create; return a removal failure after a successful close.
- Do not add fault-injection or a production seam solely to force removal failure.
- Do not change `StartDetached` or `Manager.Ensure`.

---

### Task 1: Add the RuntimeStore writability preflight

**Files:**
- Modify: `daemon/runtime.go`
- Modify: `daemon/runtime_test.go`
- Modify: `daemon/runtime_unix_test.go`
- Create: `daemon/runtime_internal_test.go`
- Include: `docs/superpowers/plans/2026-08-19-runtime-store-writability.md`

**Interfaces:**
- Consumes: `RuntimeStore.validatePrefix() (string, error)` and `RuntimeStore.prepareDir() error`.
- Produces: `func (s RuntimeStore) CheckWritable() error`.

- [x] **Step 1: Write the portable failing public-API tests**

Append these tests to `daemon/runtime_test.go`:

```go
func TestRuntimeStoreCheckWritableLeavesNoFiles(t *testing.T) {
	require := require.New(t)

	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir}

	require.NoError(store.CheckWritable())
	entries, err := os.ReadDir(dir)
	require.NoError(err)
	require.Empty(entries)
	records, err := store.List()
	require.NoError(err)
	require.Empty(records)
}

func TestRuntimeStoreCheckWritableRejectsNonDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	path := filepath.Join(t.TempDir(), "runtime-file")
	require.NoError(os.WriteFile(path, []byte("not a directory"), 0o600))

	err := (daemon.RuntimeStore{Dir: path}).CheckWritable()

	require.Error(err)
	assert.Contains(err.Error(), "prepare runtime dir")
}
```

- [x] **Step 2: Write the failing Unix permission-identity test**

Append this test to `daemon/runtime_unix_test.go`:

```go
func TestRuntimeStoreCheckWritablePreservesPermissionError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write through directory mode restrictions")
	}
	require := require.New(t)

	parent := t.TempDir()
	require.NoError(os.Chmod(parent, 0o500))
	t.Cleanup(func() { require.NoError(os.Chmod(parent, 0o700)) })

	err := (daemon.RuntimeStore{Dir: filepath.Join(parent, "runtime")}).CheckWritable()

	require.ErrorIs(err, os.ErrPermission)
}
```

- [x] **Step 3: Write the failing probe-namespace test**

Create `daemon/runtime_internal_test.go`:

```go
package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeStoreWriteCheckStaysOutsideDiscoveryNamespaces(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	store := RuntimeStore{Dir: t.TempDir(), Prefix: "tool"}
	prefix, err := store.validatePrefix()
	require.NoError(err)
	probe, err := os.CreateTemp(store.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	require.NoError(err)
	t.Cleanup(func() {
		_ = probe.Close()
		_ = os.Remove(probe.Name())
	})

	records, err := store.List()
	require.NoError(err)
	assert.Empty(records)
	name := filepath.Base(probe.Name())
	_, isRecord := pidFromName(prefix, name)
	assert.False(isRecord)
	assert.NotEqual(prefix+".lock", name)
	assert.NotEqual(prefix+".listen.lock", name)
}
```

- [x] **Step 4: Run the focused test and verify RED**

Run:

```bash
go test ./daemon -run 'TestRuntimeStoreCheckWritable|TestRuntimeStoreWriteCheck'
```

Expected: build failure because `RuntimeStore.CheckWritable` and `runtimeWriteCheckPattern` do not exist.

- [x] **Step 5: Implement the minimal public method**

Add the probe pattern near `RuntimeStore`, then add the method immediately after `prepareDir` in `daemon/runtime.go`:

```go
const runtimeWriteCheckPattern = ".%s.write-check-*"

// CheckWritable verifies that the calling process can create and remove a file
// in the runtime directory. It is an advisory preflight: it does not guarantee
// future access or that a child process has the same permissions.
func (s RuntimeStore) CheckWritable() error {
	prefix, err := s.validatePrefix()
	if err != nil {
		return err
	}
	if err := s.prepareDir(); err != nil {
		return err
	}
	probe, err := os.CreateTemp(s.Dir, fmt.Sprintf(runtimeWriteCheckPattern, prefix))
	if err != nil {
		return fmt.Errorf("create runtime write check: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		_ = os.Remove(probePath)
		return fmt.Errorf("close runtime write check: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return fmt.Errorf("remove runtime write check: %w", err)
	}
	return nil
}
```

- [x] **Step 6: Run focused tests and verify GREEN**

Run:

```bash
go test ./daemon -run 'TestRuntimeStoreCheckWritable|TestRuntimeStoreWriteCheck'
```

Expected: `ok go.kenn.io/kit/daemon`.

- [x] **Step 7: Format and run package checks**

Run:

```bash
gofmt -w daemon/runtime.go daemon/runtime_test.go daemon/runtime_unix_test.go daemon/runtime_internal_test.go
go test ./daemon
go vet ./daemon
```

Expected: all commands exit zero.

- [x] **Step 8: Run repository checks**

Run:

```bash
go test ./...
go vet ./...
```

Expected: all commands exit zero. Existing compiler deprecation warnings from the SQLite binding are acceptable if the commands still exit zero.

- [x] **Step 9: Review and commit the implementation**

Review `git diff --check`, `git diff origin/main...HEAD`, and the worktree status. Stage only the plan and daemon files, run the public-content scrub over the staged patch and commit message, then commit with a rationale-first message explaining why detached daemon callers need the preflight.
