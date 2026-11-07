package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRuntimeRecordBlocksStartForeign(t *testing.T) {
	cmd, _ := startLivePIDHelper(t)
	pid := cmd.Process.Pid
	assert.False(t, runtimeRecordBlocksStart(RuntimeRecord{PID: pid}, ProcessScopeForeign))
}
