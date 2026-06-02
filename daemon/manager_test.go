package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestManagerEnsureStartsAndPollsForCompatibleDaemon(t *testing.T) {
	assert := assert.New(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v2"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	started := false
	manager := daemon.Manager{
		Store: store,
		Discover: daemon.DiscoverOptions{
			Probe: daemon.ProbeOptions{ExpectedService: "tool"},
		},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "v2"
		},
		Start: func(context.Context) error {
			started = true
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v2",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	rec, info, err := manager.Ensure(context.Background(), time.Second)
	require.NoError(t, err)
	assert.True(started)
	assert.Equal("tool", rec.Service)
	assert.Equal("v2", info.Version)
}

func TestManagerFindSkipsIncompatibleDaemon(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, server),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	_, _, ok, err := manager.Find(context.Background())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestManagerFindScansPastIncompatibleDaemon(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)

	oldServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"old"}`)
	}))
	defer oldServer.Close()
	newServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"new"}`)
	}))
	defer newServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, oldServer),
		Service:   "tool",
		Version:   "old",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, newServer),
		Service:   "tool",
		Version:   "new",
		StartedAt: time.Now(),
	})
	require.NoError(err)

	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Compatible: func(_ daemon.RuntimeRecord, info daemon.PingInfo) bool {
			return info.Version == "new"
		},
	}

	rec, info, ok, err := manager.Find(context.Background())
	require.NoError(err)
	require.True(ok)
	assert.Equal(listenerAddr(t, newServer), rec.Address)
	assert.Equal("new", info.Version)
}

func TestManagerEnsureSerializesConcurrentStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"tool","version":"v1"}`)
	}))
	defer server.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	var starts atomic.Int32
	manager := daemon.Manager{
		Store:    store,
		Discover: daemon.DiscoverOptions{Probe: daemon.ProbeOptions{ExpectedService: "tool"}},
		Start: func(context.Context) error {
			starts.Add(1)
			time.Sleep(20 * time.Millisecond)
			_, err := store.Write(daemon.RuntimeRecord{
				PID:       os.Getpid(),
				Network:   daemon.NetworkTCP,
				Address:   listenerAddr(t, server),
				Service:   "tool",
				Version:   "v1",
				StartedAt: time.Now(),
			})
			return err
		},
	}

	errs := make(chan error, 2)
	for range 2 {
		go func() {
			_, _, err := manager.Ensure(context.Background(), time.Second)
			errs <- err
		}()
	}
	for range 2 {
		err := <-errs
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), starts.Load())
}

func TestManagerEnsureAppliesTimeoutToStartLock(t *testing.T) {
	require := require.New(t)

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	lockPath, err := store.LockPath()
	require.NoError(err)
	require.NoError(os.MkdirAll(store.Dir, 0o700))
	lock := flock.New(lockPath)
	require.NoError(lock.Lock())
	defer func() { _ = lock.Unlock() }()

	manager := daemon.Manager{
		Store: store,
		Start: func(context.Context) error {
			t.Fatal("start should not run while lock is held")
			return nil
		},
	}

	startedAt := time.Now()
	_, _, err = manager.Ensure(context.Background(), 50*time.Millisecond)
	require.Error(err)
	assert.Less(t, time.Since(startedAt), 500*time.Millisecond)
}
