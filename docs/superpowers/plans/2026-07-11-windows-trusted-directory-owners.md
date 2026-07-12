# Windows Trusted Directory Owners Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development
> (if subagents available) or superpowers:executing-plans to implement this
> plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Accept trusted Windows system principals as owners of protected
private directories without broadening current-user file ownership.

**Architecture:** Add one Windows-only helper that constructs the canonical
trusted directory-principal set and use it for both directory owner and DACL
validation. Keep `windowsOwnerMatches` unchanged as the narrow file-owner
policy, and document the distinction in package guidance.

**Tech Stack:** Go, `golang.org/x/sys/windows`, testify, Windows build tags

______________________________________________________________________

## File Structure

- `safefileio/private_dir_windows.go`: construct and apply the trusted
  directory-principal set and keep public behavior comments accurate.
- `safefileio/private_dir_windows_test.go`: protect trusted directory owners and
  the narrower file-owner policy.
- `safefileio/AGENTS.md`: record the Windows directory/file ownership invariant.

### Task 1: Protect the directory and file owner policies

**Files:**

- Modify: `safefileio/private_dir_windows_test.go`

Use @superpowers:test-driven-development and @testing-without-tautologies for
this task.

- [ ] **Step 1: Add a failing table-driven directory-owner validation test**

Add imports for `github.com/stretchr/testify/assert`, then add:

```go
func TestVerifyWindowsDirectoryOwner(t *testing.T) {
	userSID, err := currentWindowsUserSID()
	require.NoError(t, err)
	ownerSID, err := currentWindowsOwnerSID()
	require.NoError(t, err)
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	require.NoError(t, err)
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	require.NoError(t, err)
	worldSID, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	require.NoError(t, err)

	tests := []struct {
		name  string
		owner *windows.SID
		wantError bool
	}{
		{name: "current user", owner: userSID},
		{name: "token owner", owner: ownerSID},
		{name: "local system", owner: systemSID},
		{name: "administrators", owner: adminsSID},
		{name: "unrelated principal", owner: worldSID, wantError: true},
		{name: "missing owner", owner: nil, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := verifyWindowsDirectoryOwner(
				"runtime", tt.owner, userSID, ownerSID,
			)
			if tt.wantError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
```

This tests the non-privileged policy seam called by directory-handle validation.
It avoids attempting to assign Administrators or LocalSystem as a real directory
owner, which requires token privileges not available to every Windows test
process.

- [ ] **Step 2: Extend the narrow file-owner regression**

In `TestWindowsOwnerMatchesCurrentUserAndTokenOwner`, construct LocalSystem and
Administrators SIDs and assert both remain rejected by `windowsOwnerMatches`:

```go
systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
require.NoError(t, err)
adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
require.NoError(t, err)

require.False(t, windowsOwnerMatches(systemSID, userSID, ownerSID))
require.False(t, windowsOwnerMatches(adminsSID, userSID, ownerSID))
```

- [ ] **Step 3: Compile the Windows tests and verify RED**

Run:

```bash
GOOS=windows GOARCH=amd64 go test -c ./safefileio -o /tmp/kit-safefileio-red.test.exe
```

Expected: compilation fails because `verifyWindowsDirectoryOwner` is undefined.
Remove any partial output file afterward.

### Task 2: Align directory owner and DACL validation

**Files:**

- Modify: `safefileio/private_dir_windows.go`

- Modify: `safefileio/private_dir_windows_test.go`

- Modify: `safefileio/AGENTS.md`

- [ ] **Step 1: Add the canonical trusted-directory helper**

Add this helper next to `windowsAnyOwnerMatches`:

```go
func trustedWindowsDirectoryOwners(
	userSID, ownerSID *windows.SID,
) ([]*windows.SID, error) {
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		return nil, err
	}
	adminsSID, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
	if err != nil {
		return nil, err
	}
	return []*windows.SID{userSID, ownerSID, systemSID, adminsSID}, nil
}
```

- [ ] **Step 2: Use the helper for directory-owner validation**

Add the policy seam exercised by the table test:

```go
func verifyWindowsDirectoryOwner(
	path string, owner, userSID, ownerSID *windows.SID,
) error {
	trusted, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
	if err != nil {
		return err
	}
	if !windowsAnyOwnerMatches(owner, trusted) {
		return fmt.Errorf("%s is not owned by a trusted private-directory principal", path)
	}
	return nil
}
```

Replace the `windowsOwnerMatches` check in `verifyWindowsDirHandle` with:

```go
return verifyWindowsDirectoryOwner(path, owner, userSID, ownerSID)
```

The existing `TestValidatePrivateDirAcceptsPrivateDir` continues to exercise the
complete handle-validation path for a current-user-owned directory, while the
policy-seam table covers owners that cannot be portably assigned by an
unprivileged test process. `TestValidatePrivateDirRejectsBroadDACL` continues to
exercise the public rejection path for an untrusted DACL.

- [ ] **Step 3: Update the public directory behavior comments**

Update the comments on `EnsurePrivateDir` and `ValidatePrivateDir` to say that
Windows private directories may be owned by the current token user, token owner,
LocalSystem, or built-in Administrators, while retaining the requirement for a
user/system/admin-only DACL.

- [ ] **Step 4: Use the same helper for DACL validation**

Replace the duplicated LocalSystem/Administrators SID construction and `allowed`
slice in `verifyWindowsDirDACL` with:

```go
allowed, err := trustedWindowsDirectoryOwners(userSID, ownerSID)
if err != nil {
	return err
}
```

Keep the existing protected-DACL, allow-ACE, and unexpected-principal checks
unchanged.

- [ ] **Step 5: Update the package ownership invariant**

Replace the current-user-only ownership bullet in `safefileio/AGENTS.md` with:

```markdown
- Keep file ownership checks tied to current-user-only runtime state. On
  Windows, private directories may also be owned by LocalSystem or built-in
  Administrators only when their protected DACL grants access exclusively to
  the same trusted principals.
```

- [ ] **Step 6: Format and compile the Windows tests to verify GREEN**

Run:

```bash
gofmt -w safefileio/private_dir_windows.go safefileio/private_dir_windows_test.go
GOOS=windows GOARCH=amd64 go test -c ./safefileio -o /tmp/kit-safefileio-green.test.exe
rm -f /tmp/kit-safefileio-green.test.exe
```

Expected: compilation succeeds. The host cannot execute the Windows test binary;
the repository's Windows CI job will execute these build-tagged tests.

- [ ] **Step 7: Run host tests and static checks**

Run:

```bash
go test ./...
go vet ./...
git diff --check
```

Expected: all commands succeed.

- [ ] **Step 8: Commit the implementation**

Use @kenn:commit. Stage only:

```bash
git add safefileio/AGENTS.md safefileio/private_dir_windows.go safefileio/private_dir_windows_test.go
```

Commit with a rationale-first message explaining why elevated-created Windows
directories need the same trusted-principal policy already enforced by their
protected DACLs, and why file ownership remains narrower.

### Task 3: Verify and publish

Use @kenn:verify-before-handoff, @superpowers:verification-before-completion,
@kenn:scrub-private-data, and @kenn:commit-push-pr.

- [ ] **Step 1: Review the complete branch**

Run:

```bash
git status --short
git diff origin/main...HEAD
git log --format='%h %s' origin/main..HEAD
```

Confirm the branch contains only the reviewed spec, implementation plan, package
invariant, Windows implementation, and tests.

- [ ] **Step 2: Run fresh verification**

Run:

```bash
GOOS=windows GOARCH=amd64 go test -c ./safefileio -o /tmp/kit-safefileio-final.test.exe
rm -f /tmp/kit-safefileio-final.test.exe
go test ./...
go vet ./...
git diff --check
```

Expected: all commands succeed.

- [ ] **Step 3: Scrub public surfaces**

Scan the full branch diff and commit messages, plus the drafted PR and #1103
comment, using the configured private-terms denylist and structural checks.
Require zero hits before publishing.

- [ ] **Step 4: Push and open the kit PR**

Push `fix/windows-trusted-directory-owners` to `origin` and open a
rationale-first PR. Explain that Windows elevated processes can create protected
private directories owned by Administrators, that the old owner check
contradicted the existing DACL policy, and that file ownership remains
current-user-only. Do not name downstream private consumers or include a
validation transcript.

- [ ] **Step 5: Comment on agentsview PR #1103**

After the kit PR URL exists, post a concise comment explaining:

- the Administrator-owned-directory trigger is a kit owner-policy bug and is
  addressed by the linked kit PR;
- the agentsview startup-state fallback remains valuable defense-in-depth for
  runtime-record publication failures that occur after a start lock was
  successfully acquired;
- #1103 should not be treated as the root fix for the ownership case because kit
  currently gates both runtime-record and start-lock paths through the same
  directory validation.

Do not include test transcripts or private environment details.
