package embedserver

import (
	"os"
	"runtime"
	"testing"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/stretchr/testify/require"
)

func TestNativeThreadLimit(t *testing.T) {
	dir, library := os.Getenv("KIT_EMBED_TEST_DIR"), os.Getenv("KIT_EMBED_TEST_LIBRARY")
	if dir == "" || library == "" {
		t.Skip("set KIT_EMBED_TEST_DIR and KIT_EMBED_TEST_LIBRARY for native test")
	}
	host, err := cpu.Counts(true)
	require.NoError(t, err)
	if host < 8 {
		t.Skip("requires at least 8 host CPUs")
	}
	previous := runtime.GOMAXPROCS(2)
	defer runtime.GOMAXPROCS(previous)
	before, err := os.ReadDir("/proc/self/task")
	require.NoError(t, err)
	e, err := loadModel(t.Context(), dir, library)
	require.NoError(t, err)
	defer func() { require.NoError(t, e.Close()) }()
	after, err := os.ReadDir("/proc/self/task")
	require.NoError(t, err)
	require.LessOrEqual(t, len(after)-len(before), 4)
}
