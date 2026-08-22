//go:build !unix

package openssh

import (
	"context"
	"testing"

	Assert "github.com/stretchr/testify/assert"
	Require "github.com/stretchr/testify/require"
)

func TestPersistentManagerIsUnsupported(t *testing.T) {
	require := Require.New(t)
	manager, err := NewPersistentManager(t.TempDir(), PersistentConfig{})
	require.NoError(err)

	_, err = manager.Connect(context.Background(), "studio", testTarget("wes@studio"))
	require.ErrorIs(err, ErrPersistentUnsupported)
	_, err = manager.IsAlive(context.Background(), "studio", Generation(1))
	require.ErrorIs(err, ErrPersistentUnsupported)
	arguments, err := ClientArguments("")
	require.NoError(err)
	Assert.Equal(t, []string{
		"-o", "ControlMaster=no",
		"-o", "ControlPersist=no",
		"-S", "none",
	}, arguments)
}
