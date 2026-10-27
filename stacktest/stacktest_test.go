package stacktest_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/stacktest"
)

func blockedWorker(release <-chan struct{}) { <-release }

func TestDumpFailureOutput(t *testing.T) {
	if os.Getenv("STACKTEST_FAILURE_HELPER") == "1" {
		synctest.Test(t, func(t *testing.T) {
			release := make(chan struct{})
			defer close(release)
			go blockedWorker(release)
			synctest.Wait()
			stacktest.Dump(t)
			t.Log("dump returned")
			require.FailNow(t, "operation timed out")
		})
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestDumpFailureOutput$")
	cmd.Env = append(os.Environ(), "STACKTEST_FAILURE_HELPER=1")
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	require.Equal(t, 1, exit.ExitCode(), "%s", output)
	log := string(output)
	require.Contains(t, log, "--- FAIL: TestDumpFailureOutput")
	require.Contains(t, log, "stacktest: goroutine stacks")
	require.Contains(t, log, "dump returned")
	assert.Less(t, strings.Index(log, "stacktest: goroutine stacks"), strings.Index(log, "dump returned"))
	assert.Contains(t, log, "stacktest_test.blockedWorker")
	assert.Regexp(t, `stacktest_test\.go:\d+: stacktest: goroutine stacks`, log)
}
