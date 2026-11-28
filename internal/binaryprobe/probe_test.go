package binaryprobe

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProbeHelper(t *testing.T) {
	if os.Getenv("BINARYPROBE_HELPER") == "" {
		return
	}
	if os.Getenv("BINARYPROBE_HELPER") == "large" {
		_, _ = os.Stdout.WriteString(strings.Repeat("x", 17<<10))
		os.Exit(0)
	}
	_, _ = os.Stdout.WriteString("stdout message\n")
	_, _ = os.Stderr.WriteString("stderr message\n")
	if os.Getenv("BINARYPROBE_HELPER") == "fail" {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestRun(t *testing.T) {
	binary, err := os.Executable()
	require.NoError(t, err)
	t.Setenv("BINARYPROBE_HELPER", "ok")
	stdout, err := Run(t.Context(), binary, []string{"-test.run=^TestProbeHelper$"})
	require.NoError(t, err)
	assert.Equal(t, "stdout message\n", string(stdout))
	t.Setenv("BINARYPROBE_HELPER", "fail")
	_, err = Run(t.Context(), binary, []string{"-test.run=^TestProbeHelper$"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stdout message")
	assert.Contains(t, err.Error(), "stderr message")
	t.Setenv("BINARYPROBE_HELPER", "large")
	_, err = Run(t.Context(), binary, []string{"-test.run=^TestProbeHelper$"})
	require.ErrorContains(t, err, "stdout")
	require.NoError(t, RunDiscard(t.Context(), binary, []string{"-test.run=^TestProbeHelper$"}))
}

func TestOutputPrefix(t *testing.T) {
	output := &outputPrefix{limit: outputLimit}
	first := "version `GLIBC_9.99' not found\n"
	chunk := bytes.Repeat([]byte("x"), 32<<10)
	n, err := output.Write([]byte(first))
	require.NoError(t, err)
	assert.Equal(t, len(first), n)
	for range 64 {
		n, err = output.Write(chunk)
		require.NoError(t, err)
		assert.Equal(t, len(chunk), n)
	}
	assert.Len(t, output.buf, outputLimit)
	assert.True(t, strings.HasPrefix(output.String(), strings.TrimSpace(first)))
	assert.True(t, strings.HasSuffix(output.String(), "..."))
}
