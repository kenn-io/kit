package embedserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCPUQuota(t *testing.T) {
	for _, tc := range []struct {
		name, cgroup, root, path, fs, quota, period string
		want                                        int
		missing, directory, malformed               bool
	}{
		{name: "v2", cgroup: "0::/a/b", root: "/a", path: "b", fs: "cgroup2", quota: "800000 100000", want: 8},
		{name: "unlimited v2", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "max 100000"},
		{name: "unlimited v2 without period", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "max"},
		{name: "fraction", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "150000 100000", want: 2},
		{name: "v1", cgroup: "3:cpu,cpuacct:/a", root: "/", path: "a", fs: "cgroup", quota: "800000", period: "100000", want: 8},
		{name: "unlimited v1", cgroup: "3:cpu:/", root: "/", fs: "cgroup", quota: "-1", period: "100000"},
		{name: "v1 preferred", cgroup: "0::/other\n3:cpu:/", root: "/", fs: "cgroup", quota: "800000", period: "100000", want: 8},
		{name: "missing quota", cgroup: "0::/", root: "/", fs: "cgroup2", missing: true},
		{name: "missing v1 quota", cgroup: "3:cpu:/", root: "/", fs: "cgroup", missing: true},
		{name: "unreadable", cgroup: "0::/", root: "/", fs: "cgroup2", directory: true},
		{name: "bad quota", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "abc 100000", malformed: true},
		{name: "missing field", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "100000", malformed: true},
		{name: "zero quota", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "0 100000", malformed: true},
		{name: "zero period", cgroup: "0::/", root: "/", fs: "cgroup2", quota: "800000 0", malformed: true},
		{name: "boundary", cgroup: "0::/ab", root: "/a", fs: "cgroup2", quota: "800000 100000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			mount := filepath.Join(dir, "mount point")
			require.NoError(t, os.MkdirAll(filepath.Join(mount, tc.path), 0o700))
			quotaFile := "cpu.max"
			if tc.fs == "cgroup" {
				quotaFile = "cpu.cfs_quota_us"
			}
			path := filepath.Join(mount, tc.path, quotaFile)
			if tc.directory {
				require.NoError(t, os.Mkdir(path, 0o700))
			} else if !tc.missing {
				require.NoError(t, os.WriteFile(path, []byte(tc.quota), 0o600))
			}
			if tc.period != "" {
				require.NoError(t, os.WriteFile(filepath.Join(mount, tc.path, "cpu.cfs_period_us"), []byte(tc.period), 0o600))
			}
			cg := filepath.Join(dir, "cgroup")
			mi := filepath.Join(dir, "mountinfo")
			require.NoError(t, os.WriteFile(cg, []byte(tc.cgroup+"\n"), 0o600))
			escaped := strings.ReplaceAll(mount, " ", `\040`)
			require.NoError(t, os.WriteFile(mi, []byte("1 0 0:1 "+tc.root+" "+escaped+" rw - "+tc.fs+" cgroup rw,cpu\n"), 0o600))
			got, err := cpuQuota(cg, mi)
			if tc.directory || tc.malformed {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tc.want, got)
			}
		})
	}
	t.Run("missing proc file", func(t *testing.T) {
		n, err := cpuQuota(filepath.Join(t.TempDir(), "missing"), "unused")
		require.NoError(t, err)
		assert.Zero(t, n)
	})
	t.Run("malformed mount", func(t *testing.T) {
		dir := t.TempDir()
		cg, mi := filepath.Join(dir, "cgroup"), filepath.Join(dir, "mountinfo")
		require.NoError(t, os.WriteFile(cg, []byte("0::/\n"), 0o600))
		require.NoError(t, os.WriteFile(mi, []byte("bad\n"), 0o600))
		_, err := cpuQuota(cg, mi)
		require.Error(t, err)
	})
}
