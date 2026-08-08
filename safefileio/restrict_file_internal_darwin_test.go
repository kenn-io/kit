package safefileio

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveDarwinExtendedACLRejectsFailedSet(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "record-*.json")
	require.NoError(t, err)
	require.NoError(t, file.Close())

	require.Error(t, removeDarwinExtendedACL(file))
}
