package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedclient"
	"go.kenn.io/kit/embedconfig"
	"go.kenn.io/kit/embedlocal"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/internal/embedtest"
)

// CI supplies an extracted, relocated final archive. This catches missing companion
// libraries and executable-relative lookup failures that model-only parity misses.
func TestBundleNativeParity(t *testing.T) {
	executable, dir := os.Getenv("KIT_EMBED_TEST_EXECUTABLE"), os.Getenv("KIT_EMBED_TEST_DIR")
	if executable == "" || dir == "" {
		t.Skip("native bundle fixture is unset")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--data-dir", dir)
	command.Dir = t.TempDir()
	command.Env = bundleEnvironment(t.TempDir())
	stderr, err := command.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())

	drained := make(chan struct{})
	defer func() { cancel(); _ = command.Wait(); <-drained }()
	scanner := bufio.NewScanner(stderr)
	var endpoint string
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "http://") {
			endpoint = scanner.Text()
			break
		}
	}
	go func() { _, _ = io.Copy(io.Discard, stderr); close(drained) }()
	require.NotEmpty(t, endpoint, "bundle exited before publishing its endpoint")
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: endpoint},
	})
	require.NoError(t, err)

	sample := embedtest.Load(t, "../../embedserver/testdata/ollama.json")[0]
	actual, err := client.EncodeFunc(embedconfig.RoleQuery)(ctx, []string{sample.Input})
	require.NoError(t, err)
	require.Len(t, actual, 1)
	sample.AssertParity(t, actual[0])
	discovered, err := embedlocal.Endpoint(ctx, embedlocal.Options{DataDir: dir, Model: embedlocal.FaceModel})
	require.NoError(t, err)
	assert.Equal(t, endpoint, discovered)
	photo, err := os.ReadFile(filepath.Join(dir, "faces", "commons-armstrong-gemini8.jpg"))
	require.NoError(t, err)
	faces, err := client.EmbedFaces(ctx, photo, []embedclient.FaceBox{
		{X: .58, Y: 0, Width: .36, Height: .27},
	})
	require.NoError(t, err)
	require.Len(t, faces.Data, 1)
	require.NotNil(t, faces.Data[0].Embedding)
	if runtime.GOOS == "windows" {
		// Inspect the live process, so installed system copies cannot mask a missing
		// app-local dependency in the bundle under test.
		script := fmt.Sprintf("(Get-Process -Id %d).Modules | ForEach-Object { $_.FileName }", command.Process.Pid)
		output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", script).CombinedOutput()
		require.NoError(t, err, string(output))
		for _, name := range embedassets.NativeFiles() {
			assert.Contains(t, strings.ToLower(string(output)), strings.ToLower(filepath.Join(filepath.Dir(executable), name)))
		}
	}
}

func bundleEnvironment(home string) []string {
	blocked := map[string]bool{
		"HOME": true, "USERPROFILE": true, "PATH": true,
		"XDG_CONFIG_HOME": true, "XDG_CACHE_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true,
		"LD_LIBRARY_PATH": true, "LD_PRELOAD": true, "LD_AUDIT": true,
		"DYLD_LIBRARY_PATH": true, "DYLD_FRAMEWORK_PATH": true, "DYLD_INSERT_LIBRARIES": true,
		"DYLD_FALLBACK_LIBRARY_PATH": true, "DYLD_FALLBACK_FRAMEWORK_PATH": true,
	}
	env := make([]string, 0, len(os.Environ()))
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if !blocked[strings.ToUpper(name)] {
			env = append(env, entry)
		}
	}
	path := "/usr/bin:/bin"
	if runtime.GOOS == "windows" {
		path = filepath.Join(os.Getenv("SystemRoot"), "System32")
	}
	return append(env, "HOME="+home, "USERPROFILE="+home, "PATH="+path,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_CACHE_HOME="+filepath.Join(home, "cache"),
		"XDG_DATA_HOME="+filepath.Join(home, "data"), "XDG_STATE_HOME="+filepath.Join(home, "state"))
}
