package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartScope(t *testing.T) {
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: identity}
	// The identity matches the live child, so only the Foreign scope keeps it
	// from blocking: the Local branch would return true for the same record.
	require.NotEqual(t, ProcessIdentityMismatch, CompareRuntimeProcessIdentity(rec))
	require.True(t, runtimeRecordBlocksStart(rec, ProcessScopeLocal))

	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
}
