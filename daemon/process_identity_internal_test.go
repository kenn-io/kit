package daemon

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartWithScope(t *testing.T) {
	pid := os.Getppid()
	require.NotEqual(t, os.Getpid(), pid)
	require.True(t, ProcessAlive(pid))
	_, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: "future-v1:12345"}
	tests := []struct {
		name  string
		scope ProcessScope
		want  bool
	}{
		{"local mismatched identity", ProcessScopeLocal, false},
		{"unknown live PID", ProcessScopeUnknown, true},
		{"foreign live PID", ProcessScopeForeign, false},
		{"gone live PID", ProcessScopeGone, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			blocks := runtimeRecordBlocksStart(rec, func(got RuntimeRecord) ProcessScope {
				calls++
				assert.Equal(t, rec, got)
				return tt.scope
			})
			assert.Equal(t, tt.want, blocks)
			assert.Equal(t, 1, calls)
		})
	}
}
