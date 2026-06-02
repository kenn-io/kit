//go:build !windows

package daemon_test

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestListenUnixRemovesStaleSocketAndBinds(t *testing.T) {
	socketPath := staleUnixSocket(t)
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(context.Background(), ep)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	conn, err := net.DialTimeout(daemon.NetworkUnix, socketPath, time.Second)
	require.NoError(t, err)
	_ = conn.Close()
}

func TestListenUnixRejectsNonSocketPath(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	require.NoError(os.WriteFile(socketPath, []byte("not a socket"), 0o600))
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(context.Background(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "refusing to remove non-socket path")

	body, readErr := os.ReadFile(socketPath)
	require.NoError(readErr)
	assert.Equal("not a socket", string(body))
}

func TestListenUnixRejectsLiveSocket(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := unixSocketPath(t)
	live, err := net.Listen(daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}

	listener, err := daemon.Listen(context.Background(), ep)
	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon already listening")

	conn, err := net.DialTimeout(daemon.NetworkUnix, socketPath, time.Second)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixSerializesConcurrentStaleSocketStartup(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	opt := daemon.WithListenLockPath(lockPath)

	const starters = 16
	start := make(chan struct{})
	results := make(chan listenResult, starters)
	for range starters {
		go func() {
			<-start
			listener, err := daemon.Listen(context.Background(), ep, opt)
			results <- listenResult{listener: listener, err: err}
		}()
	}
	close(start)

	var winner net.Listener
	var errors []error
	for range starters {
		result := <-results
		if result.err == nil {
			require.Nil(winner, "only one daemon start should bind the socket")
			winner = result.listener
			continue
		}
		errors = append(errors, result.err)
	}
	require.NotNil(winner)
	t.Cleanup(func() { _ = winner.Close() })
	require.Len(errors, starters-1)
	for _, err := range errors {
		assert.True(
			strings.Contains(err.Error(), "daemon already listening") ||
				strings.Contains(err.Error(), "bind: address already in use"),
			"unexpected listen error: %v", err)
	}

	conn, err := net.DialTimeout(daemon.NetworkUnix, socketPath, time.Second)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixProbesAfterAcquiringLock(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	lockPath := filepath.Join(filepath.Dir(socketPath), "daemon.lock")
	heldLock := flock.New(lockPath)
	require.NoError(heldLock.Lock())
	locked := true
	t.Cleanup(func() {
		if locked {
			_ = heldLock.Unlock()
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	resultCh := make(chan listenResult, 1)
	go func() {
		listener, err := daemon.Listen(ctx, ep, daemon.WithListenLockPath(lockPath))
		resultCh <- listenResult{listener: listener, err: err}
	}()

	require.NoError(os.Remove(socketPath))
	live, err := net.Listen(daemon.NetworkUnix, socketPath)
	require.NoError(err)
	t.Cleanup(func() { _ = live.Close() })

	require.NoError(heldLock.Unlock())
	locked = false
	result := <-resultCh
	require.Error(result.err)
	assert.Nil(result.listener)
	assert.Contains(result.err.Error(), "daemon already listening")

	conn, err := net.DialTimeout(daemon.NetworkUnix, socketPath, time.Second)
	require.NoError(err)
	_ = conn.Close()
}

func TestListenUnixRejectsUnsafeLockDirectory(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	socketPath := staleUnixSocket(t)
	base, err := os.MkdirTemp("/tmp", "kitd-lock")
	require.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	target := filepath.Join(base, "target")
	link := filepath.Join(base, "link")
	require.NoError(os.MkdirAll(target, 0o700))
	require.NoError(os.Symlink(target, link))

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithListenLockPath(filepath.Join(link, "daemon.lock")))

	require.Error(err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "prepare daemon lock dir")
	assert.Contains(err.Error(), "symlink")
	_, statErr := os.Lstat(socketPath)
	require.NoError(statErr, "stale socket should not be touched when lock dir is unsafe")
}

func TestListenUnixRejectsRelativeLockPath(t *testing.T) {
	assert := assert.New(t)

	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: unixSocketPath(t)}
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithListenLockPath("daemon.lock"))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "daemon lock path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsRelativeSocketPath(t *testing.T) {
	assert := assert.New(t)

	lockPath := filepath.Join(t.TempDir(), "daemon.lock")
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: "daemon.sock"}
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithListenLockPath(lockPath))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "unix socket path")
	assert.Contains(err.Error(), "must be absolute")
}

func TestListenUnixRejectsSharedSocketDirectoryEvenWithStoreLock(t *testing.T) {
	assert := assert.New(t)

	socketPath := filepath.Join("/tmp", "kitd-shared-socket.sock")
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	ep := daemon.Endpoint{Network: daemon.NetworkUnix, Address: socketPath}
	listener, err := daemon.Listen(context.Background(), ep, daemon.WithRuntimeStore(daemon.RuntimeStore{Dir: t.TempDir()}))

	require.Error(t, err)
	assert.Nil(listener)
	assert.Contains(err.Error(), "validate unix socket dir")
	_, statErr := os.Lstat(socketPath)
	assert.True(os.IsNotExist(statErr), "socket in shared dir should not be created: %v", statErr)
}

type listenResult struct {
	listener net.Listener
	err      error
}

func staleUnixSocket(t *testing.T) string {
	t.Helper()
	socketPath := unixSocketPath(t)
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	defer func() { _ = syscall.Close(fd) }()
	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrUnix{Name: socketPath}))
	if _, err := os.Lstat(socketPath); err != nil {
		t.Fatalf("bound unix socket did not leave a socket path: %v", err)
	}
	return socketPath
}

func unixSocketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "kitd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "d.sock")
}
