package provenance

import (
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

func TestInfoJSON(t *testing.T) {
	data, err := json.Marshal(Info{SchemaVersion: 42, BuildTime: expected().BuildTime.In(time.FixedZone("offset", 3600))})
	require.NoError(t, err)
	assert.JSONEq(t, `{"schema_version":42,"build_time":"2026-01-02T04:04:05+01:00"}`, string(data))
}

func TestCheckFinalBinary(t *testing.T) {
	stamps := map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "buildTime": "2026-01-02T04:04:05+01:00", "sourceTime": sourceStamp}
	path := fixture(t, stamps)
	want := expected()
	require.NoError(t, Check(t.Context(), path, nil, want))
	uppercase := want
	uppercase.Commit = strings.ToUpper(uppercase.Commit)
	require.NoError(t, Check(t.Context(), path, nil, uppercase))
	for _, tc := range []struct {
		name   string
		change func(*Info)
	}{
		{"version with uppercase commit", func(i *Info) { i.Commit = strings.ToUpper(i.Commit); i.Version = "v1.2.3" }},
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
		{"source only", strings.Replace(valid, `"build_time":"2026-01-02T04:04:05+01:00",`, "", 1), ""},
		{"equal times", strings.Replace(valid, sourceStamp, buildStamp, 1), ""},
		{"zero build time", strings.Replace(valid, `2026-01-02T04:04:05+01:00`, `0001-01-01T00:00:00Z`, 1), "build_time must not be zero or null"},
		{"zero source time", strings.Replace(valid, sourceStamp, `0001-01-01T00:00:00Z`, 1), "source_time must not be zero or null"},
		{"malformed", "{", "decode provenance JSON"},
		{"missing schema", strings.Replace(valid, `"schema_version":1,`, "", 1), "unsupported observed schema_version: 0"},
		{"future schema", strings.Replace(valid, `"schema_version":1`, `"schema_version":2`, 1), "unsupported observed schema_version: 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rowWant := want
			switch tc.name {
			case "zero build time":
				rowWant.BuildTime = time.Time{}
			case "zero source time":
				rowWant.SourceTime = time.Time{}
			case "source only":
				rowWant.BuildTime = time.Time{}
			case "equal times":
				rowWant.SourceTime = rowWant.BuildTime
			}
			err := Check(t.Context(), path, []string{"output", tc.output}, rowWant)
			if tc.contains == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.contains)
			}
		})
	}
	require.ErrorContains(t, Check(t.Context(), path, []string{"fail"}, want), "run provenance command")

	// Exercise the release artifact built with only the version linker stamp.
	versionOnly := fixture(t, map[string]string{"version": "1.2.3"})
	out, err := exec.CommandContext(t.Context(), versionOnly).Output()
	require.NoError(t, err)
	var linked Info
	require.NoError(t, json.Unmarshal(out, &linked))
	assert.Equal(t, Info{SchemaVersion: SchemaVersion, Version: "1.2.3"}, linked)
	require.ErrorContains(t, Check(t.Context(), versionOnly, nil, want), "provenance mismatch")
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

func TestCheckBareRelativePath(t *testing.T) {
	path := fixture(t, map[string]string{"version": "1.2.3", "commit": commit, "distribution": "tap", "buildTime": buildStamp, "sourceTime": sourceStamp})
	t.Chdir(filepath.Dir(path))
	t.Setenv("PATH", t.TempDir())
	require.NoError(t, Check(t.Context(), filepath.Base(path), nil, expected()))
}
