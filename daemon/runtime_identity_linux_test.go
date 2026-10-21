//go:build linux

package daemon_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestLinuxRuntimeRecordUsesVersionedIdentityWithoutExposingItToOldClients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1234",
	})
	require.NotEmpty(rec.ProcessIdentityV2)
	assert.Empty(rec.ProcessIdentity)
	assert.Equal(daemon.ProcessIdentityMatch, daemon.CompareRuntimeProcessIdentity(rec))

	body, err := json.Marshal(rec)
	require.NoError(err)
	var legacy struct {
		ProcessIdentity daemon.ProcessIdentity `json:"process_identity"`
	}
	require.NoError(json.Unmarshal(body, &legacy))
	assert.Empty(legacy.ProcessIdentity)
}

func TestLinuxLegacyWallClockIdentityFailsClosed(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:             1,
		ProcessIdentity: "123456789",
	}
	assert.Equal(t, daemon.ProcessIdentityUnknown, daemon.CompareRuntimeProcessIdentity(rec))
}

// ownRecordWithIdentityField returns a record for this live process whose
// versioned identity has field i (1 boot ID, 2 PID namespace) replaced.
func ownRecordWithIdentityField(t *testing.T, i int, value string) daemon.RuntimeRecord {
	t.Helper()
	own, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	fields := strings.Split(string(own), ":")
	require.Len(t, fields, 4)
	fields[i] = value
	return daemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":")),
	}
}

func TestRuntimeProcessAliveIgnoresLocalPIDWhenIdentityProvesProcessGone(t *testing.T) {
	rec := ownRecordWithIdentityField(t, 1, "11111111-2222-3333-4444-555555555555")
	assert.False(t, daemon.RuntimeProcessAlive(rec))
}
