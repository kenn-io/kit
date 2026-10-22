package daemon_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestProcessIdentityMatchesTheSameLiveProcess(t *testing.T) {
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	require.NotEmpty(t, identity)
	assert.Equal(t, daemon.ProcessIdentityMatch,
		daemon.CompareProcessIdentity(os.Getpid(), identity))
}

func TestProcessIdentityRejectsAMismatchedIdentity(t *testing.T) {
	require := require.New(t)
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(ok)
	var mismatched daemon.ProcessIdentity
	if runtime.GOOS == "linux" {
		fields := strings.Split(string(identity), ":")
		require.Len(fields, 4)
		startTicks, err := strconv.ParseUint(fields[3], 10, 64)
		require.NoError(err)
		fields[3] = strconv.FormatUint(startTicks+1, 10)
		mismatched = daemon.ProcessIdentity(strings.Join(fields, ":"))
	} else {
		created, err := strconv.ParseUint(string(identity), 10, 64)
		require.NoError(err)
		mismatched = daemon.ProcessIdentity(strconv.FormatUint(created+1, 10))
	}
	assert.Equal(t, daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(os.Getpid(), mismatched))
}

func TestProcessIdentityTreatsMalformedIdentityAsUnknown(t *testing.T) {
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), "malformed"))
}

func TestRuntimeProcessIdentityTreatsUnsupportedVersionAsUnknown(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentityV2: "future-v1:12345",
	}
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareRuntimeProcessIdentity(rec))
}

func TestProcessIdentityTreatsMissingIdentityAsUnknown(t *testing.T) {
	assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), ""))
}

func TestRuntimeProcessScope(t *testing.T) {
	own := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{})
	tests := []struct {
		name string
		rec  daemon.RuntimeRecord
		want daemon.ProcessScope
	}{
		{"own record", own, daemon.ProcessScopeLocal},
		{"no versioned identity", daemon.RuntimeRecord{PID: os.Getpid()}, daemon.ProcessScopeLocal},
		{"unsupported identity", daemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: "future-v1:12345"}, daemon.ProcessScopeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daemon.RuntimeProcessScope(tt.rec))
		})
	}
}

func TestRuntimeRecordBlocksStart(t *testing.T) {
	childPID := startLivePIDHelper(t)
	identity, ok := daemon.ReadProcessIdentity(childPID)
	require.True(t, ok)
	type testCase struct {
		name string
		rec  daemon.RuntimeRecord
		want bool
	}
	tests := []testCase{
		{"self", daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{}), false},
		{"live child", daemon.RuntimeRecord{PID: childPID, ProcessIdentityV2: identity}, true},
		{"unknown live child", daemon.RuntimeRecord{PID: childPID, ProcessIdentityV2: "future-v1:12345"}, true},
		{"unknown dead PID", daemon.RuntimeRecord{PID: deadPID(t), ProcessIdentityV2: "future-v1:12345"}, false},
	}
	if runtime.GOOS == "linux" {
		fields := strings.Split(string(identity), ":")
		require.Len(t, fields, 4)
		ownNamespace := fields[2]
		fields[2] = "12345"
		foreign := daemon.RuntimeRecord{PID: childPID, ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":"))}
		require.Contains(t, []daemon.ProcessScope{daemon.ProcessScopeForeign, daemon.ProcessScopeGone}, daemon.RuntimeProcessScope(foreign))
		tests = append(tests, testCase{"live child in another namespace", foreign, false})
		fields[2] = ownNamespace
		startTicks, err := strconv.ParseUint(fields[3], 10, 64)
		require.NoError(t, err)
		fields[3] = strconv.FormatUint(startTicks+1, 10)
		tests = append(tests, testCase{"live child with mismatched start ticks", daemon.RuntimeRecord{PID: childPID, ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":"))}, false})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, daemon.RuntimeRecordBlocksStart(tt.rec))
		})
	}
}
