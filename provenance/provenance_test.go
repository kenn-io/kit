package provenance

import (
	"context"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

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
	expected := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	want := Want{Version: "v1.2.3", Commit: commit, Distribution: "tap", Time: BuildTime, ExpectedTime: expected}
	record, err := json.Marshal(Info{SchemaVersion: SchemaVersion, Version: "1.2.3", Commit: commit, Distribution: "tap", BuildTime: expected.In(time.FixedZone("offset", 3600))})
	require.NoError(t, err)
	valid := `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","build_time":"2026-01-02T03:04:05Z","distribution":"tap","extra":true}`
	cases := []struct {
		name     string
		args     []string
		want     Want
		contains []string
	}{
		{"unstamped successful executable", []string{"unstamped"}, want, []string{"version", "commit", "build_time", "distribution"}},
		{"valid", []string{"output", valid}, want, nil},
		{"Info round trip", []string{"output", string(record)}, want, nil},
		{"no distribution expectation", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","build_time":"2026-01-02T03:04:05Z"}`}, Want{Version: "1.2.3", Commit: commit, Time: BuildTime, ExpectedTime: expected}, []string{"distribution"}},
		{"no distribution report", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"distribution"}},
		{"missing expectation", []string{"output", valid}, Want{Version: "1.2.3", Commit: commit, Distribution: "tap", Time: BuildTime}, []string{"expected", "time"}},
		{"invalid time kind", []string{"output", valid}, Want{Version: "1.2.3", Commit: commit, Distribution: "tap", ExpectedTime: expected}, []string{"time kind"}},
		{"source mislabeled as build", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"2026-01-02T03:04:04Z"}`}, want, []string{"build_time"}},
		{"wrong time", []string{"output", valid}, Want{Version: "1.2.3", Commit: commit, Distribution: "tap", Time: BuildTime, ExpectedTime: expected.Add(time.Second)}, []string{"build_time"}},
		{"missing schema", []string{"output", strings.Replace(valid, `"schema_version":1,`, "", 1)}, want, []string{"schema_version"}},
		{"future schema", []string{"output", strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1)}, want, []string{"schema_version"}},
		{"wrong fields", []string{"output", `{"schema_version":1,"version":"2","commit":"abcd","distribution":"core","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"version", "commit", "distribution"}},
		{"wrong full commit", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","distribution":"tap","build_time":"2026-01-02T03:04:05Z"}`}, want, []string{"commit"}},
		{"source only", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z"}`}, want, []string{"build_time"}},
		{"source accepted", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z"}`}, Want{Version: "1.2.3", Commit: commit, Distribution: "tap", Time: SourceTime, ExpectedTime: expected}, nil},
		{"equal times", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","source_time":"2026-01-02T03:04:05Z","build_time":"2026-01-02T03:04:05Z"}`}, want, nil},
		{"offset", []string{"output", `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"2026-01-02T04:04:05+01:00"}`}, want, nil},
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

func TestVersionOnlyLinkedBinary(t *testing.T) {
	name := "fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	cmd := exec.CommandContext(t.Context(), "go", "build", "-ldflags", "-X main.version=1.2.3", "-o", binary, "./testdata/fixture")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	err = Check(t.Context(), binary, []string{"linked"}, Want{Version: "1.2.3", Commit: commit, Distribution: "archive", Time: BuildTime, ExpectedTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)})
	require.Error(t, err)
	for _, field := range []string{"commit", "distribution", "build_time"} {
		assert.Contains(t, err.Error(), field)
	}
	assert.NotContains(t, err.Error(), "version mismatch")
	assert.NotContains(t, err.Error(), "schema_version")
}

func TestCheckBareRelativePath(t *testing.T) {
	name := "fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := t.TempDir()
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", filepath.Join(dir, name), "./testdata/fixture")
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", output)
	t.Chdir(dir)
	t.Setenv("PATH", t.TempDir())
	want := Want{Version: "1.2.3", Commit: commit, Distribution: "tap", Time: BuildTime, ExpectedTime: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	record := `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"2026-01-02T03:04:05Z"}`
	require.NoError(t, Check(t.Context(), name, []string{"output", record}, want))
}
