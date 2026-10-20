//go:build linux

package daemon_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestLinuxRuntimeRecordUsesVersionedIdentityWithoutExposingItToOldClients(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	rec := daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{
		Network: daemon.NetworkTCP,
		Address: "127.0.0.1:1234",
	})
	require.NotEmpty(rec.ProcessIdentityV2)
	assert.Empty(rec.ProcessIdentity)
	assert.Equal(daemon.ProcessIdentityMatch, daemon.CompareRuntimeProcessIdentity(rec))

	body, err := json.Marshal(rec)
	require.NoError(err)
	var legacy struct {
		ProcessIdentity daemon.ProcessIdentity `json:"process_identity"`
	}
	require.NoError(json.Unmarshal(body, &legacy))
	assert.Empty(legacy.ProcessIdentity)
}

func TestLinuxLegacyWallClockIdentityFailsClosed(t *testing.T) {
	rec := daemon.RuntimeRecord{
		PID:             1,
		ProcessIdentity: "123456789",
	}
	assert.Equal(t, daemon.ProcessIdentityUnknown, daemon.CompareRuntimeProcessIdentity(rec))
}

// foreignNamespaceRecord returns a record for pid whose identity names a PID
// namespace other than this process's, as a bwrap or container server writes.
func foreignNamespaceRecord(t *testing.T, pid int, address string) daemon.RuntimeRecord {
	t.Helper()
	own, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	fields := strings.Split(string(own), ":")
	require.Len(t, fields, 4)
	fields[2] = "1" // no process shares namespace inode 1
	return daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           address,
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":")),
	}
}

// earlierBootRecord is foreignNamespaceRecord left over from a previous boot.
func earlierBootRecord(t *testing.T, pid int, address string) daemon.RuntimeRecord {
	t.Helper()
	rec := foreignNamespaceRecord(t, pid, address)
	fields := strings.Split(string(rec.ProcessIdentityV2), ":")
	fields[1] = "11111111-2222-3333-4444-555555555555"
	rec.ProcessIdentityV2 = daemon.ProcessIdentity(strings.Join(fields, ":"))
	return rec
}

func TestCleanupDeadKeepsRecordFromAnotherPIDNamespace(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	pid := deadPID(t)
	store := daemon.RuntimeStore{Dir: t.TempDir()}

	foreign := foreignNamespaceRecord(t, pid, "127.0.0.1:7474")
	foreignPath, err := store.Write(foreign)
	require.NoError(err)

	removed, err := store.CleanupDead()
	require.NoError(err)
	assert.Zero(removed)
	assert.FileExists(foreignPath)

	// The same record in this namespace is dead and goes.
	local := foreign
	local.ProcessIdentityV2 = daemon.NewRuntimeRecord("tool", "v1", daemon.Endpoint{}).ProcessIdentityV2
	_, err = store.Write(local)
	require.NoError(err)
	removed, err = store.CleanupDead()
	require.NoError(err)
	assert.Equal(1, removed)
	assert.NoFileExists(foreignPath)
}

func TestProcessIdentityForeignNamespaceIsUnknown(t *testing.T) {
	rec := foreignNamespaceRecord(t, os.Getpid(), "127.0.0.1:7474")
	assert.Equal(t, daemon.ProcessIdentityUnknown, daemon.CompareRuntimeProcessIdentity(rec))
}

func TestDiscoverProbesRecordFromAnotherPIDNamespace(t *testing.T) {
	pid := deadPID(t)

	t.Run("live server is found", func(t *testing.T) {
		server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
			Service: "tool",
			PID:     pid,
		}))
		defer server.Close()
		store := daemon.RuntimeStore{Dir: t.TempDir()}
		_, err := store.Write(foreignNamespaceRecord(t, pid, listenerAddr(t, server)))
		require.NoError(t, err)

		rec, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{
			Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
			RequirePIDAlive: true,
		})
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, pid, rec.PID)
	})

	t.Run("refused probe is unreachable and Ensure starts nothing", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		addr := listenerAddr(t, server)
		server.Close() // the port now refuses connections
		store := daemon.RuntimeStore{Dir: t.TempDir()}
		_, err := store.Write(foreignNamespaceRecord(t, pid, addr))
		require.NoError(t, err)

		var starts int
		manager := daemon.Manager{
			Store: store,
			Discover: daemon.DiscoverOptions{
				Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
				RequirePIDAlive: true,
			},
			Start: func(context.Context) error {
				starts++
				return nil
			},
		}
		_, _, err = manager.Ensure(t.Context(), time.Second)
		require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
		assert.Zero(t, starts)
	})
}

func TestRecordFromAnEarlierBootIsDead(t *testing.T) {
	pid := deadPID(t)

	t.Run("CleanupDead removes it", func(t *testing.T) {
		store := daemon.RuntimeStore{Dir: t.TempDir()}
		path, err := store.Write(earlierBootRecord(t, pid, "127.0.0.1:7474"))
		require.NoError(t, err)

		removed, err := store.CleanupDead()
		require.NoError(t, err)
		assert.Equal(t, 1, removed)
		assert.NoFileExists(t, path)
	})

	t.Run("Ensure starts a server", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		addr := listenerAddr(t, server)
		server.Close()
		store := daemon.RuntimeStore{Dir: t.TempDir()}
		_, err := store.Write(earlierBootRecord(t, pid, addr))
		require.NoError(t, err)

		var starts int
		manager := daemon.Manager{
			Store: store,
			Discover: daemon.DiscoverOptions{
				Probe:           daemon.ProbeOptions{ExpectedService: "tool"},
				RequirePIDAlive: true,
			},
			Start: func(context.Context) error {
				starts++
				return errors.New("start called")
			},
		}
		_, _, err = manager.Ensure(t.Context(), time.Second)
		require.EqualError(t, err, "start called")
		require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
		assert.Equal(t, 1, starts)
	})
}
