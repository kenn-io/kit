package main

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEmbedcheckUsage(t *testing.T) {
	oldArgs, oldFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args = oldArgs; flag.CommandLine = oldFlags })
	path := filepath.Join(t.TempDir(), "sample.unknownextension")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o600))
	t.Setenv("KIT_EMBEDCHECK_TEST_UNSET", "")
	require.NoError(t, os.Unsetenv("KIT_EMBEDCHECK_TEST_UNSET"))
	for _, args := range [][]string{{"-url", "http://127.0.0.1/v1", "-model", "example"}, {"-url", "http://127.0.0.1/v1", "-model", "example", "-dims", "2", "-api-key-env", "KIT_EMBEDCHECK_TEST_UNSET"}, {"-url", "http://127.0.0.1/v1", "-model", "example", "-dims", "2", "-media", path}} {
		flag.CommandLine = flag.NewFlagSet("embedcheck", flag.ContinueOnError)
		os.Args = append([]string{"embedcheck"}, args...)
		assert.Equal(t, 2, run())
	}
}
