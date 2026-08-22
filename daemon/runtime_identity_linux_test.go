//go:build linux

package daemon_test

import (
	"encoding/json"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestLinuxRuntimeRecordUsesVersionedIdentityWithoutExposingItToOldClients(t *testing.T) {
	assert := Assert.New(t)
	require := Require.New(t)
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
	Assert.Equal(t, daemon.ProcessIdentityUnknown, daemon.CompareRuntimeProcessIdentity(rec))
}
