package main

import (
	"os"
	"os/exec"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIRejectsUnexpectedArguments(t *testing.T) {
	if os.Getenv("KIT_EMBED_CLI_TEST") == "1" {
		os.Args = append([]string{"kennembed"}, os.Args[slices.Index(os.Args, "--")+1:]...)
		main()
		return
	}
	executable, err := os.Executable()
	require.NoError(t, err)
	cmd := exec.CommandContext(t.Context(), executable, "-test.run=^TestCLIRejectsUnexpectedArguments$", "--", "--data-dir", t.TempDir(), "unexpected", "--runtime-library", "missing-library")
	cmd.Env = append(os.Environ(), "KIT_EMBED_CLI_TEST=1")
	output, err := cmd.CombinedOutput()
	require.Error(t, err)
	assert.Contains(t, string(output), "unexpected arguments: [unexpected --runtime-library missing-library]")
}
