//go:build ignore

package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/embedassets"
)

// Run with the production file explicitly: Go excludes .github from ./....
// The native parity workflow also runs these tests before checking its archives.
func TestVerifyReleaseChecksFinalBytes(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "fixture_1.2.3_"+runtime.GOOS+"_"+runtime.GOARCH+releaseArchiveSuffix())
	data := makeReleaseTar(t, map[string][]byte{releaseExecutableName(): releaseExecutable(t), "NOTICE": []byte("fixture notice")})
	require.NoError(t, os.WriteFile(archive, data, 0o600))
	contract := writeReleaseContract(t, dir, false)
	var stdout bytes.Buffer
	require.NoError(t, verifyRelease(t.Context(), []string{"-contract", contract, "-dir", dir, "-version", "v1.2.3"}, &stdout, io.Discard))
	checksums, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("%x  %s\n", sha256.Sum256(data), filepath.Base(archive)), string(checksums))
	assert.NotEmpty(t, stdout.String())
	_, err = os.Stat(filepath.Join(dir, "fixture"))
	assert.ErrorIs(t, err, os.ErrNotExist, "validation must not extract into the artifact directory")
}

func TestVerifyReleaseRefusesMissingCompanion(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "fixture_1.2.3_"+runtime.GOOS+"_"+runtime.GOARCH+releaseArchiveSuffix())
	require.NoError(t, os.WriteFile(archive, makeReleaseTar(t, map[string][]byte{releaseExecutableName(): releaseExecutable(t), "NOTICE": []byte("fixture notice")}), 0o600))
	contract := writeReleaseContract(t, dir, true)
	require.Error(t, verifyRelease(t.Context(), []string{"-contract", contract, "-dir", dir, "-version", "1.2.3"}, io.Discard, io.Discard))
}

func TestPrepareNativeUsesRelocatedArchive(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "fixture"+releaseArchiveSuffix())
	library := []byte("synthetic companion bytes; not loaded by this extraction test")
	require.NoError(t, os.WriteFile(archive, makeReleaseTar(t, map[string][]byte{releaseExecutableName(): releaseExecutable(t), embedassets.LibraryName(): library}), 0o600))
	envFile := filepath.Join(dir, "github-env")
	require.NoError(t, prepareNative(archive, releaseExecutableName(), dir, envFile))
	env, err := os.ReadFile(envFile)
	require.NoError(t, err)
	values := map[string]string{}
	for line := range strings.SplitSeq(strings.TrimSpace(string(env)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		require.True(t, ok)
		values[key] = value
	}
	executable := values["KIT_EMBED_TEST_EXECUTABLE"]
	require.NotEmpty(t, executable)
	assert.NotEqual(t, filepath.Join(dir, "fixture"), executable)
	assert.Equal(t, filepath.Dir(executable), filepath.Dir(values["KIT_EMBED_TEST_LIBRARY"]))
	got, err := os.ReadFile(values["KIT_EMBED_TEST_LIBRARY"])
	require.NoError(t, err)
	assert.Equal(t, library, got)
	command := exec.CommandContext(t.Context(), executable)
	command.Dir = t.TempDir()
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Equal(t, "archive-fixture\n", string(output))
}

func writeReleaseContract(t *testing.T, dir string, bundle bool) string {
	t.Helper()
	kind := "single"
	var companions []string
	if bundle {
		kind = "bundle"
		companions = []string{embedassets.LibraryName()}
	}
	data, err := json.Marshal(map[string]any{"artifacts": []any{map[string]any{
		"name": "fixture_{version}_" + runtime.GOOS + "_" + runtime.GOARCH + releaseArchiveSuffix(),
		"os":   runtime.GOOS, "arch": runtime.GOARCH, "executable": releaseExecutableName(), "kind": kind,
		"baseline": "native synthetic fixture", "system_libraries": releaseSystemLibraries(runtime.GOOS),
		"runtime_files": companions, "required_files": []string{"NOTICE"},
	}}})
	require.NoError(t, err)
	path := filepath.Join(dir, "contract.json")
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

func releaseSystemLibraries(goos string) []string {
	switch goos {
	case "darwin":
		return []string{"/usr/lib/libSystem.B.dylib", "/usr/lib/libresolv.9.dylib"}
	case "windows":
		return []string{"kernel32.dll"}
	default:
		return nil
	}
}

func TestReleaseSystemLibrariesArePlatformSpecific(t *testing.T) {
	for _, tc := range []struct {
		goos string
		want []string
	}{
		{goos: "linux"},
		{goos: "darwin", want: []string{"/usr/lib/libSystem.B.dylib", "/usr/lib/libresolv.9.dylib"}},
		{goos: "windows", want: []string{"kernel32.dll"}},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			assert.Equal(t, tc.want, releaseSystemLibraries(tc.goos))
		})
	}
}

func releaseExecutable(t *testing.T) []byte {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(source, []byte("package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"archive-fixture\") }\n"), 0o600))
	path := filepath.Join(dir, "fixture")
	command := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", path, source)
	command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOENV=off", "GOWORK=off", "GOFLAGS=")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func makeReleaseTar(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var data bytes.Buffer
	if runtime.GOOS == "windows" {
		archive := zip.NewWriter(&data)
		for name, contents := range files {
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(0o755)
			file, err := archive.CreateHeader(header)
			require.NoError(t, err)
			_, err = file.Write(contents)
			require.NoError(t, err)
		}
		require.NoError(t, archive.Close())
		return data.Bytes()
	}
	compressed := gzip.NewWriter(&data)
	archive := tar.NewWriter(compressed)
	for name, contents := range files {
		require.NoError(t, archive.WriteHeader(&tar.Header{Name: "./" + name, Mode: 0o755, Size: int64(len(contents))}))
		_, err := archive.Write(contents)
		require.NoError(t, err)
	}
	require.NoError(t, archive.Close())
	require.NoError(t, compressed.Close())
	return data.Bytes()
}

func releaseArchiveSuffix() string {
	if runtime.GOOS == "windows" {
		return ".zip"
	}
	return ".tar.gz"
}
func releaseExecutableName() string {
	if runtime.GOOS == "windows" {
		return "fixture.exe"
	}
	return "fixture"
}
