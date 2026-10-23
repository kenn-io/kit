package daemon

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartScopes(t *testing.T) {
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: identity}
	t.Run("scope", func(t *testing.T) {
		fields := strings.Split(string(identity), ":")
		last := len(fields) - 1
		started, err := strconv.ParseUint(fields[last], 10, 64)
		require.NoError(t, err)
		fields[last] = strconv.FormatUint(started+1, 10)
		mismatched := RuntimeRecord{PID: pid, ProcessIdentityV2: ProcessIdentity(strings.Join(fields, ":"))}
		tests := []struct {
			name  string
			rec   RuntimeRecord
			scope ProcessScope
			want  bool
		}{
			{"local matching identity", rec, ProcessScopeLocal, true},
			{"local mismatched start ticks", mismatched, ProcessScopeLocal, false},
			{"unknown", RuntimeRecord{PID: pid, ProcessIdentityV2: "future-v1:12345"}, ProcessScopeUnknown, true},
			{"foreign", rec, ProcessScopeForeign, false},
			{"gone", rec, ProcessScopeGone, false},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				assert.Equal(t, tt.want, runtimeRecordBlocksStart(tt.rec, tt.scope))
			})
		}
	})
}
