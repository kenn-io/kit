//go:build unix

package openssh

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInspectControlSocketRejectsNonSocket(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.sock")
	require.NoError(t, os.WriteFile(path, nil, 0o600))

	_, err := inspectControlSocket(context.Background(), path)

	var securityErr *ControlPathSecurityError
	require.ErrorAs(t, err, &securityErr)
	assert.Equal(t, path, securityErr.Path)
}

func TestInspectControlSocketDistinguishesListeningAndStale(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	directory, err := os.MkdirTemp("", "kit-ssh-")
	require.NoError(err)
	t.Cleanup(func() { require.NoError(os.RemoveAll(directory)) })

	listeningPath := filepath.Join(directory, "listening.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: listeningPath, Net: "unix"})
	require.NoError(err)
	defer listener.Close()

	state, err := inspectControlSocket(context.Background(), listeningPath)
	require.NoError(err)
	assert.Equal(socketListening, state)

	stalePath := filepath.Join(directory, "stale.sock")
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: stalePath, Net: "unix"})
	require.NoError(err)
	stale.SetUnlinkOnClose(false)
	require.NoError(stale.Close())

	state, err = inspectControlSocket(context.Background(), stalePath)
	require.NoError(err)
	assert.Equal(socketStale, state)
}
