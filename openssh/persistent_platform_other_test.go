//go:build !unix

package openssh

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPersistentManagerIsUnsupported(t *testing.T) {
	manager, err := NewPersistentManager(t.TempDir(), PersistentConfig{})
	require.NoError(t, err)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.ErrorIs(t, err, ErrPersistentUnsupported)
	_, err = manager.IsAlive(context.Background(), "studio", Generation(1))
	require.ErrorIs(t, err, ErrPersistentUnsupported)
	arguments, err := ClientArguments("")
	require.NoError(t, err)
	assert.Equal(t, []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-S", "none",
	}, arguments)
}
