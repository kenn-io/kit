package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestManagerEnsureStartsAndPollsForCompatibleDaemon(t *testing.T) {
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
	assert.True(t, started)
	assert.Equal(t, "tool", rec.Service)
	assert.Equal(t, "v2", info.Version)
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
	require.NoError(t, err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, newServer),
		Service:   "tool",
		Version:   "new",
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

	rec, info, ok, err := manager.Find(context.Background())
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, listenerAddr(t, newServer), rec.Address)
	assert.Equal(t, "new", info.Version)
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

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			_, _, err := manager.Ensure(context.Background(), time.Second)
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t, int32(1), starts.Load())
}
