//go:build darwin || linux || windows

package atomicfile_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/safefileio"
)

func TestWithPrivateProducesValidatedPrivateFile(t *testing.T) {
	for name, write := range map[string]func(string, []byte, ...atomicfile.Option) error{
		"WriteFile": atomicfile.WriteFile,
		"WriteNew":  atomicfile.WriteNew,
	} {
		t.Run(name, func(t *testing.T) {
			require := require.New(t)
			dir := t.TempDir()
			target := filepath.Join(dir, "secret")

			require.NoError(write(target, []byte("token"), atomicfile.WithPrivate()))

			file, err := safefileio.OpenCurrentUserFile(target)
			require.NoError(err)
			defer func() { _ = file.Close() }()
			require.NoError(safefileio.ValidatePrivateCurrentUserFile(file))
			assert.Equal(t, "token", readString(t, target))
			assert.Equal(t, []string{"secret"}, entryNames(t, dir))
		})
	}
}
