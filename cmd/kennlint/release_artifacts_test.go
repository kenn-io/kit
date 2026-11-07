package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReleaseArtifactsArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"-version", "1"}, {"-contract", "contract.json"}} {
		var out, stderr bytes.Buffer
		require.Error(t, runReleaseArtifacts(t.Context(), args, &out, &stderr))
		require.Empty(t, out.String())
	}
}

func TestReleaseArtifactsSuccessAndFailure(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	require.NoError(t, os.WriteFile(src, []byte("package main\nfunc main(){}\n"), 0o600))
	executable := filepath.Join(dir, "app")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-trimpath", "-o", executable, src)
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64", "CGO_ENABLED=0")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	data, err := os.ReadFile(executable)
	require.NoError(t, err)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "app", Mode: 0o755, Size: int64(len(data))}))
	_, err = tw.Write(data)
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	archiveName := "app_1.2.3_linux_amd64.tar.gz"
	require.NoError(t, os.WriteFile(filepath.Join(dir, archiveName), buf.Bytes(), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(fmt.Sprintf("%x  %s\n", sha256.Sum256(buf.Bytes()), archiveName)), 0o600))
	contractPath := filepath.Join(dir, "contract.json")
	require.NoError(t, os.WriteFile(contractPath, []byte(`{"artifacts":[{"name":"app_{version}_linux_amd64.tar.gz","os":"linux","arch":"amd64","executable":"app","kind":"static","baseline":"Linux"}]}`), 0o600))
	args := []string{"-contract", contractPath, "-dir", dir, "-version", "v1.2.3"}
	var out, stderr bytes.Buffer
	require.NoError(t, runReleaseArtifacts(t.Context(), args, &out, &stderr))
	require.Equal(t, "checked 1 release artifacts\n", out.String())
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "positional argument", args: append(append([]string{}, args...), "extra"), want: "unexpected arguments"},
		{name: "unknown flag", args: append(append([]string{}, args...), "-unknown"), want: "flag provided but not defined: -unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out.Reset()
			err := runReleaseArtifacts(t.Context(), tc.args, &out, &stderr)
			require.ErrorContains(t, err, tc.want)
			require.Empty(t, out.String())
		})
	}
	for _, selector := range []string{"", "windows", "unknown"} {
		out.Reset()
		require.Error(t, runReleaseArtifacts(t.Context(), append(append([]string{}, args...), "-os", selector), &out, &stderr))
		require.Empty(t, out.String())
	}
	out.Reset()
	require.NoError(t, os.Remove(filepath.Join(dir, archiveName)))
	require.Error(t, runReleaseArtifacts(t.Context(), args, &out, &stderr))
	require.Empty(t, out.String())
}
