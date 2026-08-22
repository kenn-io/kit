package daemon_test

import (
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestProcessIdentityMatchesTheSameLiveProcess(t *testing.T) {
	identity, ok := daemon.ReadProcessIdentity(os.Getpid())
	Require.True(t, ok)
	Require.NotEmpty(t, identity)
	Assert.Equal(t, daemon.ProcessIdentityMatch,
		daemon.CompareProcessIdentity(os.Getpid(), identity))
}

func TestProcessIdentityRejectsAMismatchedIdentity(t *testing.T) {
	require := Require.New(t)
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
	Assert.Equal(t, daemon.ProcessIdentityMismatch,
		daemon.CompareProcessIdentity(os.Getpid(), mismatched))
}

func TestProcessIdentityTreatsMalformedIdentityAsUnknown(t *testing.T) {
	Assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), "malformed"))
}

func TestRuntimeProcessIdentityTreatsUnsupportedVersionAsUnknown(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentityV2: "future-v1:12345",
	}
	Assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareRuntimeProcessIdentity(rec))
}

func TestProcessIdentityTreatsMissingIdentityAsUnknown(t *testing.T) {
	Assert.Equal(t, daemon.ProcessIdentityUnknown,
		daemon.CompareProcessIdentity(os.Getpid(), ""))
}
