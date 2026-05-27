package daemon_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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
