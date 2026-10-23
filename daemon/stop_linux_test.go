//go:build linux

package daemon_test

import (
	"context"
	"net/http"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/daemon"
)

func TestStopIdentityAndProof(t *testing.T) {
	for _, mode := range []string{"no proof", "impostor", "proven"} {
		t.Run(mode, func(t *testing.T) {
			key := []byte("stop-secret-key")
			proof := newDaemonProof(t, key)
			rec := daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", ProcessIdentityV2: "future-v1:12345"}
			server, rec := startProofServer(t, rec, proof)
			defer server.Close()
			if mode == "no proof" {
				rec.Network = "tcp4"
				require.Equal(t, daemon.ProcessScopeUnknown, daemon.RuntimeProcessScope(rec))
			}
			var requests atomic.Int32
			handler := server.Config.Handler
			if mode == "impostor" {
				handler = daemon.NewPingHandler(daemon.PingHandlerOptions{PID: rec.PID, Service: rec.Service})
			}
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				handler.ServeHTTP(w, r)
			})
			rec = storedStopRecord(t, rec)
			calls := 0
			opts := daemon.StopOptions{Timeout: time.Second, Request: func(context.Context, daemon.RuntimeRecord) error { calls++; server.Close(); return nil }}
			if mode != "no proof" {
				opts.Proof = proof
			}
			err := daemon.Stop(t.Context(), rec, opts)
			switch mode {
			case "proven":
				require.NoError(t, err)
			case "no proof":
				require.ErrorIs(t, err, daemon.ErrStopProofRequired)
				require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
			default:
				require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
			}
			if mode == "proven" {
				assert.Equal(t, 1, calls)
			} else {
				assert.Zero(t, calls)
			}
			if mode == "impostor" || mode == "proven" {
				assert.Positive(t, requests.Load())
			} else {
				assert.Zero(t, requests.Load())
			}
		})
	}
}
