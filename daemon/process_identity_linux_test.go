//go:build linux

package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadLinuxProcessIdentityUsesBootAndNamespaceStartTicks(t *testing.T) {
	proc := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "sys/kernel/random"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "1"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(proc, "42"), 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(proc, "sys/kernel/random/boot_id"),
		[]byte("boot-id\n"),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(proc, "1/stat"),
		[]byte(linuxStatFixture(1, "init (namespace)", "101")),
		0o644,
	))
	require.NoError(t, os.WriteFile(
		filepath.Join(proc, "42/stat"),
		[]byte(linuxStatFixture(42, "kwt daemon (worker)", "202")),
		0o644,
	))

	identity, ok := readLinuxProcessIdentity(proc, 42)
	require.True(t, ok)
	assert.Equal(t, ProcessIdentity("linux-v1:boot-id:101:202"), identity)
}

func TestParseLinuxProcessStartTicksRejectsMalformedStat(t *testing.T) {
	_, err := parseLinuxProcessStartTicks([]byte("42 malformed"))
	require.Error(t, err)
}

func linuxStatFixture(pid int, command, startTicks string) string {
	return fmt.Sprintf(
		"%d (%s) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 %s 20",
		pid,
		command,
		startTicks,
	)
}
