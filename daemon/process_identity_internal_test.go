package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartForeignScope(t *testing.T) {
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: identity}
	require.True(t, runtimeRecordBlocksStart(rec, ProcessScopeLocal))
	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
}
