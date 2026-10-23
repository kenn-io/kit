//go:build linux

package daemon_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/daemon"
)

func TestStopUnjudgedIdentityWithDeadLocalPID(t *testing.T) {
	pid := deadPID(t)
	proof := newDaemonProof(t, []byte("stop-secret-key"))
	rec := daemon.RuntimeRecord{PID: pid, Service: "test", ProcessIdentityV2: "future-v1:12345"}
	server, rec := startProofServer(t, rec, proof)
	defer server.Close()
	rec = storedStopRecord(t, rec)
	requests := 0
	err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
		Proof:   proof,
		Timeout: time.Second,
		Request: func(_ context.Context, got daemon.RuntimeRecord) error {
			assert.Equal(t, rec, got)
			requests++
			server.Close()
			return nil
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, requests)
}

func TestStopRejectsImpostorWithRecordedPID(t *testing.T) {
	proof := newDaemonProof(t, []byte("stop-secret-key"))
	rec := daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", ProcessIdentityV2: "future-v1:12345"}
	server, rec := startProofServer(t, rec, proof)
	defer server.Close()
	var requests atomic.Int32
	handler := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: rec.PID, Service: rec.Service})
	server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		handler.ServeHTTP(w, r)
	})
	rec = storedStopRecord(t, rec)
	calls := 0
	err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
		Proof: proof, Timeout: time.Second,
		Request: func(context.Context, daemon.RuntimeRecord) error { calls++; server.Close(); return nil },
	})
	require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
	assert.Zero(t, calls)
	assert.Positive(t, requests.Load())
}

func TestStopGoneProcessEvidenceReusedPID(t *testing.T) {
	rec := daemon.NewRuntimeRecord("test", "", daemon.Endpoint{})
	rec = withLinuxIdentityField(t, rec, 3, "1")
	var probes atomic.Int32
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: rec.PID})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probes.Add(1)
		ping.ServeHTTP(w, r)
	}))
	defer server.Close()
	rec.Address = listenerAddr(t, server)
	rec = storedStopRecord(t, rec)
	requests := 0
	err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
		Proof:   newDaemonProof(t, []byte("gone-process-key")),
		Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
	})
	assert.Zero(t, requests)
	require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
	require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
	assert.EqualValues(t, 1, probes.Load())
}
