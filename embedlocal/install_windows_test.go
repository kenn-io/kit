package embedlocal

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"

	"go.kenn.io/kit/internal/embedassets"
)

func TestInstallWaitsForBusyBundle(t *testing.T) {
	client, assets, _, _ := installFixture(t, "", nil)
	// Keep the server outside the bubble, and close idle client connections
	// so synctest.Wait can observe the installer blocked on a rename retry.
	client.Transport.(*http.Transport).DisableKeepAlives = true
	synctest.Test(t, func(t *testing.T) {
		opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { return true, nil }}
		var held bool
		closed := make(chan error, 1)
		opts.Progress = func(downloaded, total int64) {
			if downloaded != total-int64(len(assets)*len(modelFixture)) {
				return
			}
			// The archive has arrived, but extraction has not started. Hold its
			// destination directory as an external reader without delete sharing.
			stages, err := filepath.Glob(filepath.Join(opts.DataDir, ".install-*"))
			require.NoError(t, err)
			require.Len(t, stages, 1)
			bundle := filepath.Join(stages[0], "bin")
			require.NoError(t, os.Mkdir(bundle, 0o700))
			path, err := windows.UTF16PtrFromString(bundle)
			require.NoError(t, err)
			handle, err := windows.CreateFile(path, windows.GENERIC_READ,
				windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
				windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
			require.NoError(t, err)
			held = true
			go func() {
				synctest.Wait()
				closed <- windows.CloseHandle(handle)
			}()
		}
		result, err := install(t.Context(), opts, client, assets)
		if held {
			require.NoError(t, <-closed)
		}
		require.NoError(t, err)
		require.True(t, held, "the fixture must hold the staged bundle before publication")
		require.NoError(t, checkBundle(filepath.Dir(result.Executable)))
		for _, asset := range assets {
			contents, err := os.ReadFile(filepath.Join(embedassets.ModelDir(result.DataDir), asset.Name))
			require.NoError(t, err)
			assert.Equal(t, modelFixture, contents)
		}
	})
}
