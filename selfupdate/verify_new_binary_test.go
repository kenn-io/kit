package selfupdate

import (
	"bytes"
	"crypto/ed25519"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/consolewindow"
)

// TestNewBinaryHelper is not a test of its own. The tests below install this
// test executable as the new binary and run it with -test.run selecting this
// function, so the update check runs a real executable on every platform.
func TestNewBinaryHelper(t *testing.T) {
	i := slices.Index(os.Args, "--new-binary")
	if i < 0 || i+1 >= len(os.Args) {
		return
	}
	switch os.Args[i+1] {
	case "fail":
		_, _ = os.Stderr.WriteString("version `GLIBC_9.99' not found\n")
		os.Exit(1)
	case "hold":
		// Start a long-lived process from the original test executable that
		// inherits this run's output pipe, then exit successfully.
		child := consolewindow.CommandContext(t.Context(), os.Args[i+2], newBinaryArgs("sleep")...)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
	case "sleep":
		time.Sleep(20 * time.Second)
	}
	os.Exit(0)
}

func newBinaryArgs(outcome string, extra ...string) []string {
	return append([]string{"-test.run=^TestNewBinaryHelper$", "--", "--new-binary", outcome}, extra...)
}

// installTestExecutable serves an archive holding this test executable and
// installs it over an existing binary with verifyArgs.
func installTestExecutable(t *testing.T, verifyArgs []string) (dstPath string, executable []byte, err error) {
	t.Helper()
	require := require.New(t)

	self, err := os.Executable()
	require.NoError(err)
	executable, err = os.ReadFile(self)
	require.NoError(err)

	binaryName := "tool"
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	tmpDir := t.TempDir()
	archivePath := filepath.Join(tmpDir, "tool_1.2.0.tar.gz")
	createTarGz(t, archivePath, []archiveEntry{{Name: binaryName, Content: string(executable), Mode: 0o755}})
	checksum, err := HashFile(archivePath)
	require.NoError(err)
	payload := SignaturePayload(SignatureMetadata{
		Owner:    "kenn",
		Repo:     "tool",
		Version:  "v1.2.0",
		Asset:    filepath.Base(archivePath),
		GOOS:     runtime.GOOS,
		GOARCH:   runtime.GOARCH,
		Checksum: checksum,
	})
	publicKey, signature := signPayload(t, payload)
	archive, err := os.ReadFile(archivePath)
	require.NoError(err)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(archive)
		case "/archive.sig":
			_, _ = w.Write(signature)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	dstDir := filepath.Join(tmpDir, "bin")
	require.NoError(os.Mkdir(dstDir, 0o755))
	dstPath = filepath.Join(dstDir, binaryName)
	require.NoError(os.WriteFile(dstPath, []byte("old-binary"), 0o755))

	client := Client{Owner: "kenn", Repo: "tool", BinaryName: "tool", TrustedPublicKeys: []ed25519.PublicKey{publicKey}}
	err = client.Install(t.Context(), &Info{
		DownloadURL:   server.URL + "/archive",
		SignatureURL:  server.URL + "/archive.sig",
		AssetName:     filepath.Base(archivePath),
		LatestVersion: "v1.2.0",
		Size:          int64(len(archive)),
		Checksum:      checksum,
	}, InstallOptions{DestinationPath: dstPath, VerifyArgs: verifyArgs})
	return dstPath, executable, err
}

func TestInstallKeepsInstalledBinaryWhenNewBinaryFailsToRun(t *testing.T) {
	t.Parallel()

	dstPath, _, err := installTestExecutable(t, newBinaryArgs("fail"))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "new binary failed to run; kept the installed binary")
	assert.Contains(t, err.Error(), "version `GLIBC_9.99' not found")
	got, readErr := os.ReadFile(dstPath)
	require.NoError(t, readErr)
	assert.Equal(t, "old-binary", string(got))
	entries, readErr := os.ReadDir(filepath.Dir(dstPath))
	require.NoError(t, readErr)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{filepath.Base(dstPath)}, names, "staged binary was not removed")
}

func TestInstallStopsWaitingForOutputHeldByADescendant(t *testing.T) {
	t.Parallel()
	self, err := os.Executable()
	require.NoError(t, err)

	dstPath, _, err := installTestExecutable(t, newBinaryArgs("hold", self))

	require.ErrorIs(t, err, exec.ErrWaitDelay)
	got, readErr := os.ReadFile(dstPath)
	require.NoError(t, readErr)
	assert.Equal(t, "old-binary", string(got))
}

func TestInstallReplacesBinaryWhenNewBinaryRuns(t *testing.T) {
	t.Parallel()

	dstPath, executable, err := installTestExecutable(t, newBinaryArgs("pass"))

	require.NoError(t, err)
	got, readErr := os.ReadFile(dstPath)
	require.NoError(t, readErr)
	assert.True(t, bytes.Equal(executable, got), "installed bytes differ from the new binary")
}
