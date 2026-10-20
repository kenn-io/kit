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

func TestReadLinuxProcessIdentityUsesInspectableTargetNamespace(t *testing.T) {
	require := require.New(t)
	proc := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(proc, "sys/kernel/random"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(proc, "42/ns"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(proc, "sys/kernel/random/boot_id"),
		[]byte("b08745a1-625b-4f8b-8ab9-0123456789ab\n"),
		0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(proc, "42/stat"),
		[]byte(linuxStatFixture(42, "kwt daemon (worker)", "202")),
		0o644,
	))
	require.NoError(os.Symlink("pid:[4026532448]", filepath.Join(proc, "42/ns/pid")))

	identity, ok := readLinuxProcessIdentity(proc, 42)
	require.True(ok)
	assert.Equal(t, ProcessIdentity("linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:202"), identity)
}

func TestReadLinuxProcessIdentityRejectsMalformedTargetNamespace(t *testing.T) {
	require := require.New(t)
	proc := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(proc, "sys/kernel/random"), 0o755))
	require.NoError(os.MkdirAll(filepath.Join(proc, "42/ns"), 0o755))
	require.NoError(os.WriteFile(
		filepath.Join(proc, "sys/kernel/random/boot_id"),
		[]byte("b08745a1-625b-4f8b-8ab9-0123456789ab\n"),
		0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(proc, "42/stat"),
		[]byte(linuxStatFixture(42, "daemon", "202")),
		0o644,
	))
	require.NoError(os.Symlink("pid:not-an-inode", filepath.Join(proc, "42/ns/pid")))

	_, ok := readLinuxProcessIdentity(proc, 42)
	assert.False(t, ok)
}

func TestLinuxProcessIdentityCompatibilityRejectsMalformedValues(t *testing.T) {
	tests := []ProcessIdentity{
		"linux-v1:",
		"linux-v1:not-a-boot-id:4026532448:202",
		"linux-v1:B08745A1-625B-4F8B-8AB9-0123456789AB:4026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:missing:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:+4026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:04026532448:202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:+202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:0202",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:0",
		"linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:202:extra",
	}
	for _, identity := range tests {
		assert.False(t, processIdentityCompatible(identity), string(identity))
		assert.Equal(t, ProcessIdentityUnknown, CompareProcessIdentity(os.Getpid(), identity), string(identity))
	}
}

func TestParseLinuxProcessStartTicksRejectsMalformedStat(t *testing.T) {
	_, err := parseLinuxProcessStartTicks([]byte("42 malformed"))
	require.Error(t, err)
}

func TestLinuxProcessIdentityForeign(t *testing.T) {
	const bootID = "b08745a1-625b-4f8b-8ab9-0123456789ab"
	withNamespace := func(inode string) ProcessIdentity {
		return ProcessIdentity("linux-v1:" + bootID + ":" + inode + ":202")
	}
	tests := []struct {
		name     string
		own      string // self ns/pid link target; empty leaves it unreadable
		identity ProcessIdentity
		want     bool
	}{
		{"own namespace", "pid:[4026532448]", withNamespace("4026532448"), false},
		{"other namespace", "pid:[4026532448]", withNamespace("4026531836"), true},
		{"other boot, own namespace", "pid:[4026532448]", ProcessIdentity("linux-v1:11111111-2222-3333-4444-555555555555:4026532448:202"), false},
		{"unreadable own namespace", "", withNamespace("4026532448"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proc := t.TempDir()
			if tt.own != "" {
				require.NoError(t, os.MkdirAll(filepath.Join(proc, "self/ns"), 0o755))
				require.NoError(t, os.Symlink(tt.own, filepath.Join(proc, "self/ns/pid")))
			}
			assert.Equal(t, tt.want, linuxProcessIdentityForeign(proc, tt.identity))
		})
	}
}

func linuxStatFixture(pid int, command, startTicks string) string {
	return fmt.Sprintf(
		"%d (%s) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 %s 20",
		pid,
		command,
		startTicks,
	)
}
