//go:build darwin || linux

package safefileio_test

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/safefileio"
	"hegel.dev/go/hegel"
)

func TestSecretLinkBoundProperty(t *testing.T) {
	// Canonicalize only the fixture prefix, so /var's own link does not consume
	// one of the deliberately generated chain's 40 hops on macOS.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	target := filepath.Join(dir, "token")
	require.NoError(t, os.WriteFile(target, []byte("secret"), 0o600))
	paths := []string{target}
	for i := 1; i <= 41; i++ {
		path := filepath.Join(dir, strconv.Itoa(i))
		require.NoError(t, os.Symlink(filepath.Base(paths[i-1]), path))
		paths = append(paths, path)
	}
	hegel.Test(t, func(ht *hegel.T) {
		hops := hegel.Draw(ht, hegel.OneOf(hegel.Integers[uint32](0, ^uint32(0)), hegel.Integers[uint32](0, 41)))
		// A longer chain must already fail at hop 41; bound fixture materialization.
		file, err := safefileio.OpenSecretFile(t.Context(), paths[min(hops, 41)])
		if hops > 40 {
			require.Error(ht, err)
			assert.Nil(ht, file)
		} else {
			require.NoError(ht, err)
			require.NoError(ht, file.Close())
		}
	}, hegel.WithTestCases(2000))
}
