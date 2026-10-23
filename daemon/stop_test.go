package daemon_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/daemon"
)

func storedStopRecord(t *testing.T, rec daemon.RuntimeRecord) daemon.RuntimeRecord {
	t.Helper()
	store := daemon.RuntimeStore{Dir: t.TempDir()}
	path, err := store.Write(rec)
	require.NoError(t, err)
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	t.Cleanup(func() { assert.FileExists(t, path) })
	return records[0]
}

func TestStopInvalidPID(t *testing.T) {
	for _, pid := range []int{0, -1} {
		t.Run(strconv.Itoa(pid), func(t *testing.T) {
			server := httptest.NewServer(daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid()}))
			defer server.Close()
			requests := 0
			err := daemon.Stop(t.Context(), daemon.RuntimeRecord{PID: pid, Address: listenerAddr(t, server)}, daemon.StopOptions{
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			require.Error(t, err)
			assert.Zero(t, requests)
		})
	}
}

func TestStopListenerReplacement(t *testing.T) {
	for _, proven := range []bool{false, true} {
		name := "replacement without proof"
		if proven {
			name = "real endpoint with proof"
		}
		t.Run(name, func(t *testing.T) {
			const secret = "stop-test-secret"
			proof := newDaemonProof(t, []byte(secret))
			server, rec := startProofServer(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test"}, proof)
			defer server.Close()
			rec = storedStopRecord(t, rec)
			handler := server.Config.Handler
			if !proven {
				server.Close()
				listener, err := (&net.ListenConfig{}).Listen(t.Context(), daemon.NetworkTCP, rec.Address)
				require.NoError(t, err)
				handler = daemon.NewPingHandler(daemon.PingHandlerOptions{PID: rec.PID, Service: rec.Service})
				server = httptest.NewUnstartedServer(nil)
				require.NoError(t, server.Listener.Close())
				server.Listener = listener
				defer server.Close()
			}
			var received atomic.Int32
			server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer "+secret {
					received.Add(1)
				}
				handler.ServeHTTP(w, r)
			})
			if !proven {
				server.Start()
			}
			calls := 0
			opts := daemon.StopOptions{Timeout: time.Second, Request: func(ctx context.Context, got daemon.RuntimeRecord) error {
				calls++
				assert.Equal(t, rec, got)
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/shutdown", nil)
				if err != nil {
					return err
				}
				req.Header.Set("Authorization", "Bearer "+secret)
				resp, err := server.Client().Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				server.Close()
				return nil
			}}
			if proven {
				opts.Proof = proof
			}
			err := daemon.Stop(t.Context(), rec, opts)
			if proven {
				require.NoError(t, err)
				assert.Equal(t, 1, calls)
				assert.EqualValues(t, 1, received.Load())
			} else {
				require.ErrorIs(t, err, daemon.ErrStopProofRequired)
				require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
				assert.Zero(t, calls)
				assert.Zero(t, received.Load())
			}
		})
	}
}

func TestStopMissingProofExitEvidence(t *testing.T) {
	failure := errors.New("exit proof failed")
	for _, result := range []string{"exited", "running", "error"} {
		for _, identity := range []daemon.ProcessIdentity{"", "future-v1:12345"} {
			t.Run(result+"/"+string(identity), func(t *testing.T) {
				if identity != "" && runtime.GOOS != "linux" {
					t.Skip("PID namespace identities are Linux-specific")
				}
				rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), ProcessIdentityV2: identity, Network: daemon.NetworkTCP, Address: "127.0.0.1:1"})
				requests, checks := 0, 0
				err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
					Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
					Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
						checks++
						assert.Equal(t, rec, got)
						if result == "error" {
							return false, failure
						}
						return result == "exited", nil
					},
				})
				assert.Zero(t, requests)
				assert.Equal(t, 1, checks)
				switch result {
				case "exited":
					require.NoError(t, err)
				case "running":
					require.ErrorIs(t, err, daemon.ErrStopProofRequired)
					require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
				case "error":
					require.ErrorIs(t, err, failure)
					assert.NotEqual(t, failure, err)
				}
			})
		}
	}
}

func TestStopEndpointWait(t *testing.T) {
	ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid(), Service: "original"})
	busy := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "busy", http.StatusServiceUnavailable) })
	for _, tc := range []struct {
		name           string
		network        string
		after          http.Handler
		closeListener  bool
		timeout        time.Duration
		callerDeadline bool
		wantErr        error
	}{
		{name: "tcp silence", network: daemon.NetworkTCP, after: ping, closeListener: true, timeout: time.Second},
		{name: "unix socket removed", network: daemon.NetworkUnix, after: ping, closeListener: true, timeout: time.Second},
		{name: "503 then silence", network: daemon.NetworkTCP, after: busy, closeListener: true, timeout: time.Second},
		{name: "replacement", network: daemon.NetworkTCP, after: daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid() + 1, Service: "original"}), timeout: time.Second},
		{name: "missing PID after shutdown request", network: daemon.NetworkTCP, after: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"ok":true}`))
		}), timeout: 150 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "negative PID after shutdown request", network: daemon.NetworkTCP, after: daemon.NewPingHandler(daemon.PingHandlerOptions{PID: -1}), timeout: 150 * time.Millisecond, wantErr: context.DeadlineExceeded},
		{name: "context with live endpoint", network: daemon.NetworkTCP, after: ping, callerDeadline: true, wantErr: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requested atomic.Bool
			var probes atomic.Int32
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requested.Load() {
					probes.Add(1)
					tc.after.ServeHTTP(w, r)
					return
				}
				ping.ServeHTTP(w, r)
			}))
			if tc.network == daemon.NetworkUnix {
				require.NoError(t, server.Listener.Close())
				listener, err := net.ListenUnix(daemon.NetworkUnix, &net.UnixAddr{Name: unixSocketPath(t), Net: daemon.NetworkUnix})
				require.NoError(t, err)
				server.Listener = listener
			}
			server.Start()
			defer server.Close()
			rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "original", Network: tc.network, Address: server.Listener.Addr().String()})
			proof := newDaemonProof(t, []byte("stop-wait-key"))
			initialPing, err := proof.NewPingHandler(rec)
			require.NoError(t, err)
			ping = initialPing
			closing := make(chan struct{})
			closed := make(chan struct{})
			calls := 0
			opts := daemon.StopOptions{
				Proof:   proof,
				Timeout: tc.timeout, Probe: daemon.ProbeOptions{ExpectedService: "original"},
				Request: func(_ context.Context, got daemon.RuntimeRecord) error {
					assert.Equal(t, rec, got)
					calls++
					requested.Store(true)
					if tc.closeListener {
						time.AfterFunc(100*time.Millisecond, func() { //nolint:kennlint // models delayed shutdown on a real socket, rather than a test deadline
							close(closing)
							_ = server.Listener.Close()
							close(closed)
						})
					}
					return nil
				},
			}
			if tc.network == daemon.NetworkUnix {
				opts.Proof = nil
			}
			ctx := t.Context()
			if tc.callerDeadline {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 150*time.Millisecond) //nolint:kennlint // exercises the caller's deadline while the real endpoint remains live
				defer cancel()
			}
			err = daemon.Stop(ctx, rec, opts)
			require.ErrorIs(t, err, tc.wantErr)
			if tc.closeListener {
				select {
				case <-closing:
				default:
					assert.Fail(t, "Stop returned before listener closure began")
				}
				// Windows AF_UNIX can refuse connections before Close returns.
				// Wait for the callback before checking its socket cleanup.
				<-closed
				if tc.network == daemon.NetworkUnix {
					assert.NoFileExists(t, rec.Address)
				}
			}
			assert.Equal(t, 1, calls)
			assert.Positive(t, probes.Load())
		})
	}
}

func TestStopExitProofAndErrors(t *testing.T) {
	failure := errors.New("shutdown failed")
	for _, mode := range []string{"closed", "exited with live endpoint", "replacement", "exit error", "request error", "missing request"} {
		t.Run(mode, func(t *testing.T) {
			var requested atomic.Bool
			var probes atomic.Int32
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid()})
			replacement := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid() + 1})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if requested.Load() {
					probes.Add(1)
					if mode == "replacement" {
						replacement.ServeHTTP(w, r)
						return
					}
				}
				ping.ServeHTTP(w, r)
			}))
			defer server.Close()
			rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Address: listenerAddr(t, server)})
			proof := newDaemonProof(t, []byte("stop-exit-key"))
			var err error
			ping, err = proof.NewPingHandler(rec)
			require.NoError(t, err)
			checks := 0
			opts := daemon.StopOptions{
				Proof:   proof,
				Timeout: 150 * time.Millisecond,
				Request: func(context.Context, daemon.RuntimeRecord) error {
					requested.Store(true)
					if mode == "closed" {
						server.Close()
					}
					if mode == "request error" {
						return failure
					}
					return nil
				},
				Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
					assert.True(t, requested.Load())
					assert.Equal(t, rec, got)
					checks++
					if mode == "exit error" {
						return false, failure
					}
					return mode != "replacement" && (mode != "closed" || checks == 2), nil
				},
			}
			if mode == "request error" || mode == "missing request" {
				opts.Exited = nil
			}
			if mode == "missing request" {
				opts.Request = nil
			}
			err = daemon.Stop(t.Context(), rec, opts)
			switch mode {
			case "replacement":
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case "request error", "exit error":
				require.ErrorIs(t, err, failure)
			case "missing request":
				require.EqualError(t, err, "daemon stop request is required")
			default:
				require.NoError(t, err)
			}
			assert.Zero(t, probes.Load())
			if mode == "closed" {
				assert.Equal(t, 2, checks)
			}
		})
	}
}

func TestStopPreflightExitProof(t *testing.T) {
	failure := errors.New("exit proof failed")
	for _, endpoint := range []string{"closed", "another PID", "gone process"} {
		for _, result := range []string{"exited", "running", "error"} {
			t.Run(endpoint+"/"+result, func(t *testing.T) {
				var probes atomic.Int32
				ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: os.Getpid() + 1})
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					probes.Add(1)
					ping.ServeHTTP(w, r)
				}))
				defer server.Close()
				rec := storedStopRecord(t, daemon.RuntimeRecord{PID: os.Getpid(), Service: "test", Address: listenerAddr(t, server)})
				if endpoint == "gone process" {
					rec.PID = deadPID(t)
				}
				if endpoint == "closed" {
					server.Close()
				}
				requests, checks := 0, 0
				err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
					Proof:   newDaemonProof(t, []byte("preflight-key")),
					Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
					Exited: func(_ context.Context, got daemon.RuntimeRecord) (bool, error) {
						checks++
						assert.Equal(t, rec, got)
						if endpoint == "another PID" {
							assert.Positive(t, probes.Load(), "preflight must precede exit proof")
						}
						if result == "error" {
							return false, failure
						}
						return result == "exited", nil
					},
				})
				assert.Zero(t, requests)
				if endpoint == "gone process" {
					assert.Zero(t, probes.Load())
				}
				assert.Equal(t, 1, checks)
				switch result {
				case "exited":
					require.NoError(t, err)
				case "running":
					if endpoint == "gone process" {
						require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
						require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
					} else {
						require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
					}
				case "error":
					require.ErrorIs(t, err, failure)
					assert.NotEqual(t, failure, err, "exit proof errors must be wrapped")
				}
			})
		}
	}
}

func TestStopGoneProcessProbeContext(t *testing.T) {
	pid := deadPID(t)
	for _, mode := range []string{"canceled", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var probes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				if mode == "canceled" {
					cancel()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			rec := daemon.RuntimeRecord{PID: pid, Address: listenerAddr(t, server)}
			require.Equal(t, daemon.ProcessScopeLocal, daemon.RuntimeProcessScope(rec))
			require.False(t, daemon.RuntimeProcessAlive(rec))
			requests := 0
			err := daemon.Stop(ctx, rec, daemon.StopOptions{
				Timeout: 100 * time.Millisecond,
				Probe:   daemon.ProbeOptions{Timeout: time.Second},
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.EqualValues(t, 1, probes.Load())
			assert.Zero(t, requests)
			want := context.Canceled
			if mode == "timeout" {
				want = context.DeadlineExceeded
			}
			require.ErrorIs(t, err, want)
			assert.NotErrorIs(t, err, daemon.ErrExitUnconfirmed)
		})
	}
}

func TestStopGoneProcessEvidence(t *testing.T) {
	pid := deadPID(t)
	for _, tc := range []struct {
		name    string
		live    bool
		closed  bool
		samePID bool
		reused  bool
		wantErr bool
	}{
		{name: "recorded PID answering", samePID: true, wantErr: true},
		{name: "closed endpoint", closed: true},
		{name: "gone"},
		{name: "refused", live: true, closed: true, wantErr: true},
		{name: "another PID", live: true, wantErr: true},
		{name: "reused", live: true, samePID: true, reused: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := daemon.RuntimeRecord{PID: pid, Service: "test"}
			if tc.live {
				rec.PID = os.Getpid()
			}
			if tc.reused {
				if runtime.GOOS != "linux" {
					t.Skip("PID namespace identities are Linux-specific")
				}
				identity, ok := daemon.ReadProcessIdentity(rec.PID)
				require.True(t, ok)
				fields := strings.Split(string(identity), ":")
				require.Len(t, fields, 4)
				fields[3] = "1"
				rec.ProcessIdentityV2 = daemon.ProcessIdentity(strings.Join(fields, ":"))
			}
			answerPID := os.Getpid() + 1
			if tc.samePID {
				answerPID = rec.PID
			}
			var probes atomic.Int32
			ping := daemon.NewPingHandler(daemon.PingHandlerOptions{PID: answerPID})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				ping.ServeHTTP(w, r)
			}))
			defer server.Close()
			rec.Address = listenerAddr(t, server)
			rec = storedStopRecord(t, rec)
			if tc.closed {
				server.Close()
			}
			requests := 0
			err := daemon.Stop(t.Context(), rec, daemon.StopOptions{
				Proof:   newDaemonProof(t, []byte("gone-process-key")),
				Request: func(context.Context, daemon.RuntimeRecord) error { requests++; return nil },
			})
			assert.Zero(t, requests)
			if tc.wantErr {
				if !tc.live || tc.reused {
					require.ErrorIs(t, err, daemon.ErrExitUnconfirmed)
					require.NotErrorIs(t, err, daemon.ErrDaemonUnreachable)
				} else {
					require.ErrorIs(t, err, daemon.ErrDaemonUnreachable)
				}
			} else {
				require.NoError(t, err)
			}
			if tc.closed {
				assert.Zero(t, probes.Load())
			} else {
				assert.EqualValues(t, 1, probes.Load())
			}
		})
	}
}
