//go:build unix

package packstore

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestPreflightRejectsFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pack-fifo")
	require.NoError(t, unix.Mkfifo(path, 0o600))
	done := make(chan error, 1)
	go func() {
		reader, err := OpenMaintenancePack(path, DefaultLimits())
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(time.Second):
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		<-done
		assert.Fail(t, "bounded pack preflight blocked opening a FIFO")
	}
}
