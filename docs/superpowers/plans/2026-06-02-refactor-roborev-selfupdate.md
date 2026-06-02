# Refactor Roborev Self Update Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace roborev's duplicated `internal/update` implementation with a thin wrapper around `go.kenn.io/kit/selfupdate`.

**Architecture:** Keep `internal/update` as roborev's compatibility boundary so `cmd/roborev/update.go` and `cmd/roborev/tui/fetch.go` keep calling `CheckForUpdate`, `PerformUpdate`, and `FormatSize`. The wrapper configures kit with roborev's current release discovery behavior: GitHub HTML `/releases/latest` redirect, constructed download URLs, `SHA256SUMS`, tar.gz assets on every platform, and unsigned checksum verification.

**Tech Stack:** Go 1.26.3, `go.kenn.io/kit/selfupdate`, `github.com/stretchr/testify`, Cobra CLI, Bubble Tea TUI.

---

## File Structure

- Modify `go.mod` and `go.sum` to use a kit version that contains `selfupdate.Client.UseGitHubLatestRedirect`, `selfupdate.Client.GitHubWebBaseURL`, and `selfupdate.TarGzAssetName`.
- Modify `internal/update/update.go` into a small adapter over `go.kenn.io/kit/selfupdate`.
- Modify `internal/update/update_test.go` to keep behavior tests and remove white-box tests for deleted local helpers.
- Check `cmd/roborev/update.go` and `cmd/roborev/tui/fetch.go`; they should not need API changes because the wrapper preserves exported names.

### Task 1: Add The Failing Kit Metadata Test

**Files:**
- Modify: `internal/update/update_test.go`

- [ ] **Step 1: Replace the release discovery test with a kit-shaped expectation**

Replace `TestUpdaterCheckForUpdateUsesHTMLRedirect` with this test. It intentionally asserts fields that the old local `UpdateInfo` does not expose, so it should fail before the wrapper becomes a kit `selfupdate.Info` alias.

```go
func TestUpdaterCheckForUpdateUsesKitLatestRedirectDiscovery(t *testing.T) {
	const releaseTag = "v1.3.0"
	const assetName = "roborev_1.3.0_darwin_arm64.tar.gz"
	const checksum = "abc123def456789012345678901234567890123456789012345678901234abcd"

	downloadURL := fmt.Sprintf("https://github.com/roborev-dev/roborev/releases/download/%s/%s", releaseTag, assetName)
	checksumsURL := fmt.Sprintf("https://github.com/roborev-dev/roborev/releases/download/%s/SHA256SUMS", releaseTag)

	updater := NewUpdater(Deps{
		Client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.String() {
				case "https://github.com/roborev-dev/roborev/releases/latest":
					resp := newHTTPResponse(http.StatusFound, "")
					resp.Header.Set("Location", "https://github.com/roborev-dev/roborev/releases/tag/"+releaseTag)
					resp.Request = req
					return resp, nil
				case downloadURL:
					require.Equal(t, http.MethodHead, req.Method)
					resp := newHTTPResponse(http.StatusOK, "")
					resp.ContentLength = 42
					return resp, nil
				case checksumsURL:
					require.Equal(t, http.MethodGet, req.Method)
					return newHTTPResponse(http.StatusOK, fmt.Sprintf("%s  %s\n", checksum, assetName)), nil
				default:
					return nil, fmt.Errorf("unexpected request to %s", req.URL.String())
				}
			}),
		},
		Now:      func() time.Time { return time.Unix(0, 0) },
		Version:  "v1.2.0",
		GOOS:     "darwin",
		GOARCH:   "arm64",
		CacheDir: t.TempDir,
	})

	info, err := updater.CheckForUpdate(true)
	require.NoError(t, err)
	require.NotNil(t, info)
	assert.Equal(t, "roborev-dev", info.Owner)
	assert.Equal(t, "roborev", info.Repo)
	assert.Equal(t, "darwin", info.GOOS)
	assert.Equal(t, "arm64", info.GOARCH)
	assert.Equal(t, "v1.2.0", info.CurrentVersion)
	assert.Equal(t, releaseTag, info.LatestVersion)
	assert.Equal(t, assetName, info.AssetName)
	assert.Equal(t, downloadURL, info.DownloadURL)
	assert.Equal(t, int64(42), info.Size)
	assert.Equal(t, checksum, info.Checksum)
	assert.False(t, info.IsDevBuild)
}
```

- [ ] **Step 2: Run the focused test and confirm it fails**

Run:

```bash
go test ./internal/update -run TestUpdaterCheckForUpdateUsesKitLatestRedirectDiscovery -count=1
```

Expected: compile failure mentioning `info.Owner`, `info.Repo`, `info.GOOS`, or `info.GOARCH` is undefined on the current `UpdateInfo`.

### Task 2: Replace The Local Updater With A Kit Wrapper

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`
- Replace: `internal/update/update.go`
- Modify: `internal/update/update_test.go`

- [ ] **Step 1: Update kit dependency**

Run this after the kit support branch has merged to `main`:

```bash
go get go.kenn.io/kit@main
go mod tidy
```

Expected: `go.mod` still contains `go.kenn.io/kit`, and `go.sum` records the selected pseudo-version or tagged version from `main`.

- [ ] **Step 2: Replace `internal/update/update.go`**

Replace the file with this adapter:

```go
package update

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"go.kenn.io/kit/selfupdate"

	"go.kenn.io/roborev/internal/config"
	"go.kenn.io/roborev/internal/version"
)

const (
	releaseOwner  = "roborev-dev"
	releaseRepo   = "roborev"
	binaryBase    = "roborev"
	cacheFileName = "update_check.json"
)

type UpdateInfo = selfupdate.Info

type Reporter interface {
	Stepf(format string, args ...any)
	Progress(downloaded, total int64)
}

type Deps struct {
	Client           *http.Client
	Now              func() time.Time
	Version          string
	GOOS             string
	GOARCH           string
	CacheDir         func() string
	Executable       func() (string, error)
	GitHubWebBaseURL string
}

type Updater struct {
	deps Deps
}

type stdoutReporter struct {
	out        io.Writer
	progressFn func(downloaded, total int64)
}

type nopReporter struct{}

func CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	return defaultUpdater().CheckForUpdate(forceCheck)
}

func PerformUpdate(info *UpdateInfo, progressFn func(downloaded, total int64)) error {
	return defaultUpdater().PerformUpdate(info, stdoutReporter{
		out:        os.Stdout,
		progressFn: progressFn,
	})
}

func RestartDaemon() error {
	return nil
}

func GetCacheDir() string {
	return config.DataDir()
}

func NewUpdater(deps Deps) *Updater {
	if deps.Client == nil {
		deps.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Version == "" {
		deps.Version = version.Version
	}
	if deps.GOOS == "" {
		deps.GOOS = runtime.GOOS
	}
	if deps.GOARCH == "" {
		deps.GOARCH = runtime.GOARCH
	}
	if deps.CacheDir == nil {
		deps.CacheDir = config.DataDir
	}
	if deps.Executable == nil {
		deps.Executable = os.Executable
	}
	return &Updater{deps: deps}
}

func defaultUpdater() *Updater {
	return NewUpdater(Deps{})
}

func (u *Updater) CheckForUpdate(forceCheck bool) (*UpdateInfo, error) {
	if selfupdate.IsDevBuildVersion(u.deps.Version) && !forceCheck {
		return nil, nil
	}
	return u.client().Check(context.Background(), selfupdate.CheckOptions{
		Force:  forceCheck,
		GOOS:   u.deps.GOOS,
		GOARCH: u.deps.GOARCH,
	})
}

func (u *Updater) PerformUpdate(info *UpdateInfo, reporter Reporter) error {
	reporter = normalizeReporter(reporter)
	if info == nil {
		return fmt.Errorf("update info is nil")
	}
	if info.Checksum == "" {
		return fmt.Errorf("no checksum available for %s - refusing to install unverified binary", info.AssetName)
	}

	installDir, err := u.installDir()
	if err != nil {
		return err
	}
	binaryName := executableName(u.deps.GOOS)
	dstPath := filepath.Join(installDir, binaryName)

	reporter.Stepf("Downloading %s...\n", info.AssetName)
	if err := u.client().Install(context.Background(), info, selfupdate.InstallOptions{
		DestinationPath:   dstPath,
		ArchiveBinaryName: binaryName,
		Progress:          reporter.Progress,
	}); err != nil {
		return err
	}
	reporter.Stepf("Installing %s... OK\n", binaryName)
	return nil
}

func (u *Updater) client() selfupdate.Client {
	return selfupdate.Client{
		Owner:                   releaseOwner,
		Repo:                    releaseRepo,
		BinaryName:              binaryBase,
		CurrentVersion:          u.deps.Version,
		CacheDir:                u.deps.CacheDir(),
		HTTPClient:              u.deps.Client,
		Clock:                   u.deps.Now,
		GitHubWebBaseURL:        u.deps.GitHubWebBaseURL,
		CacheFileName:           cacheFileName,
		UseGitHubLatestRedirect: true,
		AssetName:               selfupdate.TarGzAssetName,
		AllowUnsignedChecksums:  true,
	}
}

func (u *Updater) installDir() (string, error) {
	currentExe, err := u.deps.Executable()
	if err != nil {
		return "", fmt.Errorf("find current executable: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return "", fmt.Errorf("resolve symlinks: %w", err)
	}
	return filepath.Dir(currentExe), nil
}

func executableName(goos string) string {
	if goos == "windows" {
		return binaryBase + ".exe"
	}
	return binaryBase
}

func FormatSize(bytes int64) string {
	return selfupdate.FormatSize(bytes)
}

func normalizeReporter(reporter Reporter) Reporter {
	if reporter == nil {
		return nopReporter{}
	}
	return reporter
}

func (r stdoutReporter) Stepf(format string, args ...any) {
	if r.out == nil {
		return
	}
	fmt.Fprintf(r.out, format, args...)
}

func (r stdoutReporter) Progress(downloaded, total int64) {
	if r.progressFn != nil {
		r.progressFn(downloaded, total)
	}
}

func (nopReporter) Stepf(string, ...any) {}

func (nopReporter) Progress(int64, int64) {}
```

- [ ] **Step 3: Prune obsolete white-box tests**

In `internal/update/update_test.go`, delete tests for helpers that no longer live in roborev:

```text
TestSanitizeTarPath
TestExtractTarGzPathTraversal
TestExtractTarGzSymlinkSkipped
TestExtractTarGzExistingSymlinkDoesNotEscapeDestination
TestExtractChecksum
TestExtractBaseSemver
TestIsDevBuildVersion
TestIsNewer
TestParsedVersionCompare
TestResolveLatestTag
TestFetchContentLength
```

Also delete helper functions used only by those removed tests:

```text
skipUnlessTargetOS
```

- [ ] **Step 4: Update the cache test helper**

Replace `writeCachedCheck` with this helper so the test no longer depends on roborev's removed private `cachedCheck` type:

```go
func writeCachedCheck(t *testing.T, cacheDir, cachedVersion string, checkedAt time.Time) {
	t.Helper()
	data, err := json.Marshal(struct {
		CheckedAt time.Time `json:"checked_at"`
		Version   string    `json:"version"`
	}{
		CheckedAt: checkedAt,
		Version:   cachedVersion,
	})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(cacheDir, cacheFileName), data, 0o600))
}
```

Update the cache test call to:

```go
writeCachedCheck(t, cacheDir, "v1.2.3", now.Add(-15*time.Minute))
```

- [ ] **Step 5: Update install output assertions**

In `TestUpdaterPerformUpdateInstallsBinary`, keep the archive setup and install assertions, but replace the reporter step assertions with:

```go
assert.Contains(t, reporter.steps.String(), "Downloading")
assert.Contains(t, reporter.steps.String(), "Installing "+binaryName+"... OK")
assert.NotEmpty(t, reporter.progress)
```

- [ ] **Step 6: Run update package tests**

Run:

```bash
go test ./internal/update -count=1
```

Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/update/update.go internal/update/update_test.go
git commit -m "refactor: use kit selfupdate in roborev"
```

### Task 3: Verify CLI And TUI Consumers

**Files:**
- Inspect: `cmd/roborev/update.go`
- Inspect: `cmd/roborev/tui/fetch.go`

- [ ] **Step 1: Confirm call sites still compile**

Run:

```bash
go test ./cmd/roborev ./cmd/roborev/tui -count=1
```

Expected: PASS. If either package fails because an `UpdateInfo` field moved, keep the field read the same and fix the adapter, not the CLI or TUI.

- [ ] **Step 2: Search for stale update internals**

Run:

```bash
rg 'resolveLatestTag|githubLatestReleaseURL|githubReleaseDownloadBase|extractChecksum|extractTarGz|sanitizeTarPath|fetchContentLength|parseVersion' internal/update cmd/roborev
```

Expected: no matches, except comments in deleted diff context are gone.

- [ ] **Step 3: Commit only if this task changed files**

```bash
git add cmd/roborev/update.go cmd/roborev/tui/fetch.go internal/update/update.go internal/update/update_test.go
git commit -m "test: verify update command consumers"
```

### Task 4: Final Verification

**Files:**
- Inspect: `go.mod`
- Inspect: `go.sum`
- Inspect: `internal/update/update.go`
- Inspect: `internal/update/update_test.go`

- [ ] **Step 1: Format**

Run:

```bash
go fmt ./...
```

Expected: no output.

- [ ] **Step 2: Vet**

Run:

```bash
go vet ./...
```

Expected: no output and exit code 0.

- [ ] **Step 3: Full test suite**

Run:

```bash
go test ./...
```

Expected: PASS.

- [ ] **Step 4: Repo lint**

Run:

```bash
make lint-ci
```

Expected: PASS with zero warnings.

- [ ] **Step 5: Final diff check**

Run:

```bash
git diff --stat HEAD
git diff HEAD -- internal/update/update.go internal/update/update_test.go cmd/roborev/update.go cmd/roborev/tui/fetch.go go.mod go.sum
```

Expected: the diff only replaces local updater mechanics with kit calls and preserves the CLI/TUI user workflow.

- [ ] **Step 6: Commit verification cleanup if formatting changed files**

```bash
git add go.mod go.sum internal/update/update.go internal/update/update_test.go cmd/roborev/update.go cmd/roborev/tui/fetch.go
git commit -m "chore: format roborev selfupdate migration"
```
