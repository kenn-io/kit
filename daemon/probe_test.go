package daemon_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestNewPingHandlerEmitsRequiredPingInfo(t *testing.T) {
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{
		Service: "roborev",
		Version: "dev",
		PID:     123,
	}))
	defer server.Close()

	resp, err := server.Client().Get(server.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "application/json", resp.Header.Get("Content-Type"))
	var info daemon.PingInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&info))
	assert.True(t, info.OK)
	assert.Equal(t, "roborev", info.Service)
	assert.Equal(t, "dev", info.Version)
	assert.Equal(t, 123, info.PID)
}

func TestNewPingHandlerRejectsNonGET(t *testing.T) {
	server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{}))
	defer server.Close()

	resp, err := server.Client().Post(server.URL, "application/json", nil)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, http.MethodGet, resp.Header.Get("Allow"))
}

func TestProbeHTTPRequiresOKTrue(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, daemon.DefaultPingPath, r.URL.Path)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"roborev","version":"dev","pid":123}`)
	}))
	defer server.Close()

	info, err := daemon.ProbeHTTP(context.Background(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "roborev",
	})
	require.NoError(t, err)
	assert.True(t, info.OK)
	assert.Equal(t, "roborev", info.Service)
	assert.Equal(t, "dev", info.Version)
	assert.Equal(t, 123, info.PID)
}

func TestProbeHTTPRejectsOKOmitted(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(context.Background(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPRejectsOKFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":false,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(context.Background(), server.Client(), server.URL, daemon.ProbeOptions{
		ExpectedService: "kata",
	})
	require.Error(t, err)
}

func TestProbeHTTPAppliesTimeoutOption(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	}))
	defer server.Close()

	_, err := daemon.ProbeHTTP(context.Background(), server.Client(), server.URL, daemon.ProbeOptions{
		Timeout: time.Millisecond,
	})
	require.Error(t, err)
}

func TestDiscoverFindsResponsiveRuntime(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"v1"}`)
	}))
	defer server.Close()

	addr := listenerAddr(t, server)
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   addr,
		Service:   "kata",
		Version:   "v1",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	rec, info, ok, err := daemon.Discover(context.Background(), store, daemon.DiscoverOptions{
		Probe:           daemon.ProbeOptions{ExpectedService: "kata"},
		RequirePIDAlive: true,
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, addr, rec.Address)
	assert.Equal(t, "kata", info.Service)
}

func TestDiscoverPropagatesContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, _, _, err := daemon.Discover(ctx, daemon.RuntimeStore{Dir: t.TempDir()}, daemon.DiscoverOptions{})
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoverSkipsPerProbeTimeouts(t *testing.T) {
	slowServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"slow"}`)
	}))
	defer slowServer.Close()
	fastServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata","version":"fast"}`)
	}))
	defer fastServer.Close()

	store := daemon.RuntimeStore{Dir: t.TempDir()}
	_, err := store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid(),
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, slowServer),
		Service:   "kata",
		StartedAt: time.Now().Add(-time.Minute),
	})
	require.NoError(t, err)
	_, err = store.Write(daemon.RuntimeRecord{
		PID:       os.Getpid() + 1,
		Network:   daemon.NetworkTCP,
		Address:   listenerAddr(t, fastServer),
		Service:   "kata",
		StartedAt: time.Now(),
	})
	require.NoError(t, err)

	_, info, ok, err := daemon.Discover(context.Background(), store, daemon.DiscoverOptions{
		Probe: daemon.ProbeOptions{ExpectedService: "kata", Timeout: time.Millisecond},
	})
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "fast", info.Version)
}

func TestProbeDialsUnixEndpoint(t *testing.T) {
	socketDir, err := os.MkdirTemp("", "kitd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(socketDir) })
	socketPath := filepath.Join(socketDir, "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	})

	server := http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"ok":true,"service":"kata"}`)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })

	_, err = daemon.Probe(context.Background(), daemon.Endpoint{
		Network: daemon.NetworkUnix,
		Address: socketPath,
	}, daemon.ProbeOptions{ExpectedService: "kata"})
	require.NoError(t, err)
}

func listenerAddr(t *testing.T, server *httptest.Server) string {
	t.Helper()
	addr := server.Listener.Addr().String()
	require.NoError(t, func() error {
		_, _, err := net.SplitHostPort(addr)
		return err
	}(), "server address %q", addr)
	return addr
}
