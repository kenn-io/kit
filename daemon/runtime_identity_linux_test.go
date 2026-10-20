//go:build linux

package daemon_test

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"syscall"
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

// foreignPIDNamespace returns the inode of a PID namespace other than this
// process's that a live process holds, so no caller can prove it gone. It
// starts a child in a new PID namespace and skips where the host forbids that.
func foreignPIDNamespace(t *testing.T) string {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("no sleep binary to hold a PID namespace: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), sleep, "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWPID,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("can't start a child in a new PID namespace: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	identity, ok := daemon.ReadProcessIdentity(cmd.Process.Pid)
	if !ok {
		t.Skip("can't read the PID namespace of a child in a new PID namespace")
	}
	return strings.Split(string(identity), ":")[2]
}

// foreignNamespaceRecord returns a record for pid whose identity names a PID
// namespace other than this process's, as a bwrap or container server writes.
func foreignNamespaceRecord(t *testing.T, pid int, address string) daemon.RuntimeRecord {
	t.Helper()
	namespace := foreignPIDNamespace(t)
	own, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	fields := strings.Split(string(own), ":")
	require.Len(t, fields, 4)
	fields[2] = namespace
	return daemon.RuntimeRecord{
		PID:               pid,
		Network:           daemon.NetworkTCP,
		Address:           address,
		Service:           "tool",
		StartedAt:         time.Now(),
		ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":")),
	}
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
}

// ownRecordWithIdentityField returns a record for this live process whose
// versioned identity has field i (1 boot ID, 2 PID namespace) replaced.
func ownRecordWithIdentityField(t *testing.T, i int, value string) daemon.RuntimeRecord {
	t.Helper()
	own, ok := daemon.ReadProcessIdentity(os.Getpid())
	require.True(t, ok)
	fields := strings.Split(string(own), ":")
	require.Len(t, fields, 4)
	fields[i] = value
	return daemon.RuntimeRecord{
		PID:               os.Getpid(),
		ProcessIdentityV2: daemon.ProcessIdentity(strings.Join(fields, ":")),
	}
}

func TestRuntimeProcessAliveIgnoresLocalPIDWhenIdentityProvesProcessGone(t *testing.T) {
	t.Run("earlier boot", func(t *testing.T) {
		rec := ownRecordWithIdentityField(t, 1, "11111111-2222-3333-4444-555555555555")
		assert.False(t, daemon.RuntimeProcessAlive(rec))
	})
	t.Run("own namespace takes the local PID check", func(t *testing.T) {
		rec := ownRecordWithIdentityField(t, 3, "1")
		assert.True(t, daemon.RuntimeProcessAlive(rec))
	})
	t.Run("no versioned identity takes the local PID check", func(t *testing.T) {
		assert.True(t, daemon.RuntimeProcessAlive(daemon.RuntimeRecord{PID: os.Getpid()}))
	})
}
