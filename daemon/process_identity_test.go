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
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	rec := mismatchedRuntimeIdentity(t, daemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity})
	assert.Equal(t, daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(os.Getpid(), rec.ProcessIdentityV2))
}

func mismatchedRuntimeIdentity(t *testing.T, rec daemon.RuntimeRecord) daemon.RuntimeRecord {
	t.Helper()
	identity := string(rec.ProcessIdentityV2)
	if runtime.GOOS == "linux" {
		startTicks, err := strconv.ParseUint(identity[strings.LastIndex(identity, ":")+1:], 10, 64)
		require.NoError(t, err)
		return withLinuxIdentityField(t, rec, 3, strconv.FormatUint(startTicks+1, 10))
	}
	created, err := strconv.ParseUint(identity, 10, 64)
	require.NoError(t, err)
	rec.ProcessIdentityV2 = daemon.ProcessIdentity(strconv.FormatUint(created+1, 10))
	return rec
}

func withLinuxIdentityField(t *testing.T, rec daemon.RuntimeRecord, field int, value string) daemon.RuntimeRecord {
	t.Helper()
	fields := strings.Split(string(rec.ProcessIdentityV2), ":")
	require.Len(t, fields, 4)
	fields[field] = value
	rec.ProcessIdentityV2 = daemon.ProcessIdentity(strings.Join(fields, ":"))
	return rec
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
	tests := []struct {
		name string
		rec  daemon.RuntimeRecord
		want daemon.ProcessScope
	}{
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
	pid := daemon.StartLivePIDHelperForTest(t)
	identity, ok := daemon.ReadProcessIdentity(pid)
	require.True(t, ok)
	rec := daemon.RuntimeRecord{PID: pid, ProcessIdentityV2: identity}
	type testCase struct {
		name  string
		rec   daemon.RuntimeRecord
		want  bool
		scope daemon.ProcessScope
	}
	tests := []testCase{
		{"self", daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{}), false, daemon.ProcessScopeLocal},
		{"live child", rec, true, daemon.ProcessScopeLocal},
		{"mismatched live child", mismatchedRuntimeIdentity(t, rec), false, daemon.ProcessScopeLocal},
		{"unknown live child", daemon.RuntimeRecord{PID: pid, ProcessIdentityV2: "future-v1:12345"}, true, daemon.ProcessScopeUnknown},
		{"unknown dead PID", daemon.RuntimeRecord{PID: deadPID(t), ProcessIdentityV2: "future-v1:12345"}, false, daemon.ProcessScopeUnknown},
	}
	if runtime.GOOS == "linux" {
		tests = append(tests, testCase{"earlier boot live child", withLinuxIdentityField(t, rec, 1, "11111111-2222-3333-4444-555555555555"), false, daemon.ProcessScopeGone})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.scope, daemon.RuntimeProcessScope(tt.rec))
			assert.Equal(t, tt.want, daemon.RuntimeRecordBlocksStart(tt.rec))
		})
	}
}
