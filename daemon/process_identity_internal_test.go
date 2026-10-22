package daemon

import (
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRuntimeRecordBlocksStartScope(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("PID namespaces are Linux-specific")
	}
	pid := startLivePIDHelper(t)
	identity, ok := ReadProcessIdentity(pid)
	require.True(t, ok)
	fields := strings.Split(string(identity), ":")
	require.Len(t, fields, 4)
	fields[2] = "12345"
	rec := RuntimeRecord{PID: pid, ProcessIdentityV2: ProcessIdentity(strings.Join(fields, ":"))}

	assert.False(t, runtimeRecordBlocksStart(rec, ProcessScopeForeign))
}
