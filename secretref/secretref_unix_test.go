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

func TestRefFileOpenReasons(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		mode                    os.FileMode
		missing, writableParent bool
		reason                  string
	}{
		{"bad mode", 0o666, false, false, "file must be a regular file with trusted ownership, trusted permissions and parents"},
		{"writable parent", 0o600, false, true, "parent directory is writable by group or others"},
		{"unreadable", 0, false, false, "file is unreadable"},
		{"missing", 0, true, false, "file is missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "unreadable" && os.Geteuid() == 0 {
				t.Skip("root can read the file")
			}
			dir := privateTempDir(t)
			path := filepath.Join(dir, "token")
			if !tc.missing {
				writePrivateFile(t, path, "secret")
				require.NoError(t, os.Chmod(path, tc.mode))
			}
			if tc.writableParent {
				require.NoError(t, os.Chmod(dir, 0o775))
			}
			_, err := (secretref.Ref{File: path}).Resolve()
			var source *secretref.SourceError
			require.ErrorAs(t, err, &source)
			require.Equal(t, tc.reason, source.Reason)
		})
	}
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
