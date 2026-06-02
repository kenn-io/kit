//go:build !windows

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUnixSocketStaleTreatsMissingSocketAsStale(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "kitd-probe")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	stale, err := unixSocketStale(context.Background(), filepath.Join(dir, "missing.sock"), 50*time.Millisecond)

	require.NoError(t, err)
	assert.True(t, stale)
}
