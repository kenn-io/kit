package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/embedserver"
)

func TestRunReportsFailureOnce(t *testing.T) {
	for _, tc := range []struct {
		name, flag, value, message string
	}{
		{"owned", "-library", filepath.Join(t.TempDir(), "missing"), "ONNX Runtime library"},
		{"before ownership", "-listen", "0.0.0.0:1", "loopback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := os.CreateTemp(t.TempDir(), "stderr")
			require.NoError(t, err)
			defer output.Close()
			stderr := os.Stderr
			os.Stderr = output
			defer func() { os.Stderr = stderr }()
			assert.Equal(t, 1, run([]string{"-data-dir", t.TempDir(), tc.flag, tc.value}, embedserver.Serve))
			printed, err := os.ReadFile(output.Name())
			require.NoError(t, err)
			assert.Equal(t, 1, strings.Count(string(printed), tc.message), string(printed))
		})
	}
}

func TestRun(t *testing.T) {
	for _, tt := range []struct {
		name      string
		args      []string
		serveErr  error
		wantCode  int
		wantCalls int
		wantOpts  embedserver.Options
	}{
		{name: "help", args: []string{"-h"}},
		{name: "unknown flag", args: []string{"-unknown"}, wantCode: 2},
		{name: "positional argument", args: []string{"model"}, wantCode: 2},
		{
			name:      "configured options",
			args:      []string{"-listen", "127.0.0.1:8080", "-data-dir", "data", "-library", "runtime.so"},
			wantCalls: 1,
			wantOpts:  embedserver.Options{Listen: "127.0.0.1:8080", DataDir: "data", Library: "runtime.so"},
		},
		{name: "defaults", wantCalls: 1},
		{name: "serve error", serveErr: errors.New("serve failed"), wantCode: 1, wantCalls: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			code := run(tt.args, func(_ context.Context, opts embedserver.Options) error {
				calls++
				assert.Equal(t, tt.wantOpts, opts)
				return tt.serveErr
			})
			assert.Equal(t, tt.wantCode, code)
			assert.Equal(t, tt.wantCalls, calls)
		})
	}
}
