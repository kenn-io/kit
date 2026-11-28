package provenance

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const commit = "0123456789abcdef0123456789abcdef01234567"

func TestInfoJSON(t *testing.T) {
	data, err := json.Marshal(Info{})
	require.NoError(t, err)
	assert.JSONEq(t, `{}`, string(data))
}

func TestCheckFinalBinary(t *testing.T) {
	name := "fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./testdata/fixture")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	want := Want{Version: "v1.2.3", Commit: commit, Distribution: "tap", Time: BuildTime}
	valid := `{"version":"1.2.3","commit":"` + commit + `","build_time":"2026-01-02T03:04:05Z","distribution":"tap","extra":true}`
	cases := []struct {
		name     string
		args     []string
		want     Want
		contains []string
	}{
		{"unstamped successful executable", []string{"unstamped"}, want, []string{"version", "commit", "build_time", "distribution"}},
		{"valid", []string{"output", valid}, want, nil},
		{"no distribution", []string{"output", `{"version":"1.2.3","commit":"` + commit + `","build_time":"2026-01-02T03:04:05Z"}`}, Want{Version: "1.2.3", Commit: commit, Time: BuildTime}, nil},
		{"wrong fields", []string{"output", `{"version":"2","commit":"abcd","distribution":"core","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"version", "commit", "distribution"}},
		{"wrong full commit", []string{"output", `{"version":"1.2.3","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","distribution":"tap","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"commit"}},
		{"source only", []string{"output", `{"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z"}`}, want, []string{"build_time"}},
		{"source accepted", []string{"output", `{"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z"}`}, Want{Version: "1.2.3", Commit: commit, Distribution: "tap", Time: SourceTime}, nil},
		{"equal times", []string{"output", `{"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"build_time", "source_time"}},
		{"offset", []string{"output", `{"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"2026-01-02T04:04:05+01:00"}`}, want, []string{"build_time"}},
		{"malformed", []string{"output", "{"}, want, []string{"JSON"}},
		{"nonzero", []string{"fail"}, want, []string{"fixture failed"}},
		{"oversized", []string{"large"}, want, []string{"stdout"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(t.Context(), binary, tc.args, tc.want)
			if len(tc.contains) == 0 {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			for _, s := range tc.contains {
				assert.Contains(t, err.Error(), s)
			}
		})
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, Check(ctx, binary, []string{"unstamped"}, want), context.Canceled)
}
