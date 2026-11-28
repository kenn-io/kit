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

const (
	commit      = "0123456789abcdef0123456789abcdef01234567"
	buildStamp  = "2026-01-02T03:04:05Z"
	sourceStamp = "2026-01-01T03:04:05Z"
)

func fixture(t *testing.T, stamps map[string]string) string {
	t.Helper()
	name := "fixture"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(t.TempDir(), name)
	var flags []string
	for k, v := range stamps {
		flags = append(flags, "-X", "main."+k+"="+v)
	}
	cmd := exec.CommandContext(t.Context(), "go", "build", "-ldflags", strings.Join(flags, " "), "-o", path, "./testdata/fixture")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)
	return path
}

func expected() Info {
	build, _ := time.Parse(time.RFC3339, buildStamp)
	source, _ := time.Parse(time.RFC3339, sourceStamp)
	return Info{SchemaVersion: SchemaVersion, Version: "1.2.3", Commit: commit, Distribution: "tap", BuildTime: build, SourceTime: source}
}

func TestCheckRejectsWrongSecondaryTimestamp(t *testing.T) {
	path := fixture(t, map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "buildTime": buildStamp, "sourceTime": buildStamp})
	err := Check(t.Context(), path, nil, expected())
	require.ErrorContains(t, err, "provenance mismatch")
	assert.Contains(t, err.Error(), sourceStamp)
	assert.Contains(t, err.Error(), buildStamp)
}

func TestCheckRejectsExplicitZeroTimestamp(t *testing.T) {
	path := fixture(t, nil)
	for _, tc := range []struct {
		name, output string
		want         Info
	}{
		{"build time", `"build_time":"0001-01-01T00:00:00Z","source_time":"` + sourceStamp + `"`, func() Info { i := expected(); i.BuildTime = time.Time{}; return i }()},
		{"source time", `"build_time":"` + buildStamp + `","source_time":"0001-01-01T00:00:00Z"`, func() Info { i := expected(); i.SourceTime = time.Time{}; return i }()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output := `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap",` + tc.output + `}`
			require.Error(t, Check(t.Context(), path, []string{"output", output}, tc.want))
		})
	}
}

func TestInfoJSON(t *testing.T) {
	data, err := json.Marshal(Info{SchemaVersion: 42, BuildTime: expected().BuildTime.In(time.FixedZone("offset", 3600))})
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":42,"build_time":"2026-01-02T04:04:05+01:00"}`, string(data))
}

func TestFutureSchemaRoundTripRemainsUnsupported(t *testing.T) {
	input := `{"schema_version":2,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"` + buildStamp + `","source_time":"` + sourceStamp + `","future_field":true}`
	var info Info
	require.NoError(t, json.Unmarshal([]byte(input), &info))
	output, err := json.Marshal(info)
	require.NoError(t, err)
	assert.Contains(t, string(output), `"schema_version":2`)
	want := expected()
	path := fixture(t, nil)
	require.ErrorContains(t, Check(t.Context(), path, []string{"output", string(output)}, want), "unsupported observed schema_version: 2")
}

func TestCheckFinalBinary(t *testing.T) {
	stamps := map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "buildTime": "2026-01-02T04:04:05+01:00", "sourceTime": sourceStamp}
	path := fixture(t, stamps)
	want := expected()
	require.NoError(t, Check(t.Context(), path, nil, want))
	uppercase := want
	uppercase.Commit = strings.ToUpper(uppercase.Commit)
	require.NoError(t, Check(t.Context(), path, nil, uppercase))
	uppercase.Version = "different"
	err := Check(t.Context(), path, nil, uppercase)
	require.ErrorContains(t, err, "provenance mismatch")
	assert.Contains(t, err.Error(), `"commit":"`+commit+`"`)
	for _, tc := range []struct {
		name   string
		change func(*Info)
	}{
		{"version", func(i *Info) { i.Version = "v1.2.3" }},
		{"commit", func(i *Info) { i.Commit = strings.Repeat("a", 40) }},
		{"distribution", func(i *Info) { i.Distribution = "core" }},
		{"build time", func(i *Info) { i.BuildTime = i.BuildTime.Add(time.Second) }},
		{"source time", func(i *Info) { i.SourceTime = i.SourceTime.Add(time.Second) }},
		{"unexpected build time", func(i *Info) { i.BuildTime = time.Time{} }},
		{"swapped times", func(i *Info) { i.BuildTime, i.SourceTime = i.SourceTime, i.BuildTime }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrong := want
			tc.change(&wrong)
			err := Check(t.Context(), path, nil, wrong)
			require.ErrorContains(t, err, "provenance mismatch")
			assert.Contains(t, err.Error(), " got:")
			assert.Contains(t, err.Error(), "want:")
		})
	}
	valid := `{"schema_version":1,"version":"1.2.3","commit":"` + commit + `","distribution":"tap","build_time":"2026-01-02T04:04:05+01:00","source_time":"` + sourceStamp + `","extra":true}`
	for _, tc := range []struct{ name, output, contains string }{
		{"reordered offset additive", valid, ""},
		{"case variant", strings.Replace(valid, `"version"`, `"Version"`, 1), ""},
		{"malformed", "{", "decode provenance JSON"},
		{"missing schema", strings.Replace(valid, `"schema_version":1,`, "", 1), "unsupported observed schema_version: 0"},
		{"future schema", strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), "unsupported observed schema_version: 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Check(t.Context(), path, []string{"output", tc.output}, want)
			if tc.contains == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.contains)
			}
		})
	}
	require.ErrorContains(t, Check(t.Context(), path, []string{"fail"}, want), "fixture failed")
	require.ErrorContains(t, Check(t.Context(), path, []string{"large", valid}, want), "stdout exceeds")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.ErrorIs(t, Check(ctx, path, nil, want), context.Canceled)
}

func TestLinkedArtifacts(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stamps map[string]string
		want   Info
		valid  bool
	}{
		{"version only", map[string]string{"version": "1.2.3"}, expected(), false},
		{"unstamped", nil, expected(), false},
		{"source only", map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "sourceTime": sourceStamp}, func() Info { i := expected(); i.BuildTime = time.Time{}; return i }(), true},
		{"equal times", map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "buildTime": buildStamp, "sourceTime": buildStamp}, func() Info { i := expected(); i.SourceTime = i.BuildTime; return i }(), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fixture(t, tc.stamps)
			err := Check(t.Context(), path, nil, tc.want)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "provenance mismatch")
				assert.Contains(t, err.Error(), `"schema_version":1`)
				assert.Contains(t, err.Error(), "want:")
			}
		})
	}
}

func TestInvalidExpectations(t *testing.T) {
	for _, schema := range []int{0, 2} {
		want := expected()
		want.SchemaVersion = schema
		require.ErrorContains(t, Check(t.Context(), "nonexistent", nil, want), "invalid expectation: unsupported schema_version")
	}
	for _, change := range []func(*Info){func(i *Info) { i.Version = "" }, func(i *Info) { i.Commit = "short" }, func(i *Info) { i.Distribution = "" }, func(i *Info) { i.BuildTime = time.Time{}; i.SourceTime = time.Time{} }} {
		want := expected()
		change(&want)
		require.ErrorContains(t, Check(t.Context(), "nonexistent", nil, want), "invalid expectation")
	}
}
