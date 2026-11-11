//go:build unix

package secretref_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/secretref"
)

func TestRefFilePermissionPolicy(t *testing.T) {
	dir := privateTempDir(t)
	shared := filepath.Join(dir, "shared.key")
	require.NoError(t, os.WriteFile(shared, []byte("secret"), 0o600))
	require.NoError(t, os.Chmod(shared, 0o666))
	_, err := (secretref.Ref{File: shared}).Resolve()
	require.ErrorContains(t, err, "file must be a regular file with trusted ownership")
}

func TestRefFileRefusesAFIFOWithoutBlocking(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "key.pipe")
	require.NoError(t, syscall.Mkfifo(path, 0o600))
	done := make(chan error, 1)
	go func() {
		_, err := secretref.Ref{File: path}.Resolve()
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "trusted permissions")
	case <-time.After(5 * time.Second):
		// A blocked open waits for a FIFO writer, kernel state that synctest
		// cannot observe, so this wait is wall-clock. Open a writer to
		// release the goroutine before failing.
		writer, err := os.OpenFile(path, os.O_RDWR, 0o600)
		require.NoError(t, err)
		require.NoError(t, writer.Close())
		<-done
		require.FailNow(t, "resolving a FIFO key file must not block")
	}
}

func TestUnreadableFileReportsAccessFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read the file")
	}
	path := filepath.Join(privateTempDir(t), "token")
	writePrivateFile(t, path, "secret")
	require.NoError(t, os.Chmod(path, 0))
	_, err := (secretref.Ref{File: path}).Resolve()
	var sourceErr *secretref.SourceError
	require.ErrorAs(t, err, &sourceErr)
	require.Equal(t, "file is unreadable", sourceErr.Reason)
}
