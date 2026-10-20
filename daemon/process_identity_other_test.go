//go:build !linux

package daemon

import (
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessIdentityCompatibilityRejectsValuesOutsideEmissionDomain(t *testing.T) {
	assert.True(t, processIdentityCompatible(ProcessIdentity(strconv.FormatInt(math.MaxInt64, 10))))
	assert.False(t, processIdentityCompatible("9223372036854775808"))
}

func TestCleanupDeadKeepsLinuxIdentityThisBuildCannotRead(t *testing.T) {
	const pid = 999999
	if ProcessAlive(pid) {
		t.Skipf("pid %d is alive on this host", pid)
	}
	store := RuntimeStore{Dir: t.TempDir()}
	path, err := store.Write(RuntimeRecord{
		PID:               pid,
		Network:           NetworkTCP,
		Address:           "127.0.0.1:7474",
		ProcessIdentityV2: "linux-v1:b08745a1-625b-4f8b-8ab9-0123456789ab:4026532448:202",
	})
	require.NoError(t, err)

	removed, err := store.CleanupDead()
	require.NoError(t, err)
	assert.Zero(t, removed)
	assert.FileExists(t, path)
}
