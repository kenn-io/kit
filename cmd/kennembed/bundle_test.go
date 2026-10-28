package main

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"fmt"
	"math"
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
)

// CI supplies an extracted native bundle. This catches missing companion
// libraries and executable-relative lookup failures that model-only parity misses.
func TestBundleNativeParity(t *testing.T) {
	executable, dir := os.Getenv("KIT_EMBED_TEST_EXECUTABLE"), os.Getenv("KIT_EMBED_TEST_DIR")
	if executable == "" || dir == "" {
		t.Skip("native bundle fixture is unset")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "--data-dir", dir)
	stderr, err := command.StderrPipe()
	require.NoError(t, err)
	require.NoError(t, command.Start())
	urls := make(chan string, 1)
	scanned := make(chan struct{})
	defer func() { cancel(); _ = command.Wait(); <-scanned }()
	go func() {
		defer close(scanned)
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if strings.HasPrefix(scanner.Text(), "http://") {
				urls <- scanner.Text()
				return
			}
		}
	}()
	var endpoint string
	select {
	case endpoint = <-urls:
	case <-scanned:
		select {
		case endpoint = <-urls:
		default:
			require.FailNow(t, "bundle exited before publishing its endpoint")
		}
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	client, err := embedclient.New(embedclient.Options{
		Model:      embedconfig.Model{Name: "nomic-embed-text", Dimensions: 768, Metric: embedconfig.MetricCosine, Normalization: embedconfig.NormalizationNone},
		Deployment: embedconfig.Deployment{BaseURL: endpoint},
	})
	require.NoError(t, err)
	data, err := os.ReadFile("../../embedserver/testdata/ollama.json")
	require.NoError(t, err)
	var fixture struct {
		Samples []struct {
			Input  string    `json:"input"`
			Vector []float32 `json:"vector"`
		} `json:"samples"`
	}
	require.NoError(t, json.Unmarshal(data, &fixture))
	for _, sample := range fixture.Samples {
		actual, err := client.EncodeFunc(embedconfig.RoleQuery)(ctx, []string{sample.Input})
		require.NoError(t, err)
		require.Len(t, actual, 1)
		require.Len(t, actual[0], len(sample.Vector))
		var dot, left, right float64
		for i, value := range actual[0] {
			x, y := float64(value), float64(sample.Vector[i])
			dot += x * y
			left += x * x
			right += y * y
		}
		assert.GreaterOrEqual(t, dot/math.Sqrt(left*right), 0.999)
	}
	if runtime.GOOS == "windows" {
		// Inspect the live process, so installed system copies cannot mask a missing
		// app-local dependency in the bundle under test.
		script := fmt.Sprintf("(Get-Process -Id %d).Modules | ForEach-Object { $_.FileName }", command.Process.Pid)
		output, err := exec.CommandContext(ctx, "powershell.exe", "-NoProfile", "-Command", script).CombinedOutput()
		require.NoError(t, err, string(output))
		for _, name := range []string{"onnxruntime.dll", "msvcp140.dll", "msvcp140_1.dll", "vcruntime140.dll", "vcruntime140_1.dll"} {
			assert.Contains(t, strings.ToLower(string(output)), strings.ToLower(filepath.Join(filepath.Dir(executable), name)))
		}
	}
}
