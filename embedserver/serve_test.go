package embedserver

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestServeDiscoveryAndCancellation(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	stderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = stderr; writer.Close(); reader.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered, canceled, finished, release := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	finish := sync.OnceFunc(func() { close(release) })
	defer finish()
	closed := make(chan struct{})
	closedAfterInference := false
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		close(canceled)
		<-release
		close(finished)
		return nil, 0, ctx.Err()
	}, onClose: func() {
		close(closed)
		select {
		case <-finished:
			closedAfterInference = true
		default:
		}
	}}
	store, record, done := startServe(t, ctx, e, daemon.Endpoint.ListenContext)
	requestDone := startEmbeddingRequest(t, record)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "inference did not start")
	}
	cancel()
	require.ErrorIs(t, <-requestDone, io.EOF)
	<-canceled
	select {
	case <-closed:
		require.FailNow(t, "model closed before inference returned")
	default:
	}
	finish()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancellation did not drain inference")
	}
	assert.True(t, closedAfterInference)
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, "Loading nomic-embed-text model...\n"+record.Endpoint().BaseURL()+"/v1\n", string(printed))
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestServeRejectsStartup(t *testing.T) {
	for _, tc := range []struct {
		name, listen, message string
		loaded                bool
	}{
		{"load failure", "", "model unavailable", true},
		{"non-loopback", "0.0.0.0:0", "not loopback", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			err := serve(t.Context(), Options{DataDir: t.TempDir(), Listen: tc.listen}, func(context.Context, string, string) (engine, error) {
				called = true
				return nil, errors.New("model unavailable")
			}, daemon.Endpoint.ListenContext)
			require.ErrorContains(t, err, tc.message)
			assert.Equal(t, tc.loaded, called)
		})
	}
}

func TestServeRejectsSecondOwner(t *testing.T) {
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kit-embed"}
	release, err := store.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	defer release()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	loaded := false
	err = serve(ctx, Options{DataDir: store.Dir}, func(context.Context, string, string) (engine, error) {
		loaded = true
		return nil, errors.New("model must not load")
	}, daemon.Endpoint.ListenContext)
	require.ErrorContains(t, err, "cannot own "+store.Dir+", another kennembed may be running")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.NoError(t, ctx.Err(), "owner lock wait exceeded its startup timeout")
	assert.False(t, loaded)
}

func TestServeCancellationInterruptsResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var block atomic.Bool
	writing := make(chan *blockedConn, 1)
	listen := func(ep daemon.Endpoint, ctx context.Context) (net.Listener, error) {
		listener, err := ep.ListenContext(ctx)
		return &blockedListener{Listener: listener, block: &block, writing: writing}, err
	}
	_, record, done := startServe(t, ctx, new(fakeEngine), listen)
	block.Store(true)
	requestDone := startEmbeddingRequest(t, record)
	var conn *blockedConn
	select {
	case conn = <-writing:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "response did not reach blocked Write")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "blocked response prevented shutdown")
	}
	select {
	case <-conn.closed:
	default:
		assert.Fail(t, "shutdown did not close the connection blocked in Write")
	}
	require.ErrorIs(t, <-requestDone, io.EOF)
}

func TestServeRejectsBrowserRequests(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	_, record, done := startServe(t, ctx, new(fakeEngine), daemon.Endpoint.ListenContext)
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "server did not stop")
		}
	})
	_, port, err := net.SplitHostPort(record.Endpoint().Address)
	require.NoError(t, err)
	attackerHost := net.JoinHostPort("attacker.example", port)
	for _, tc := range []struct {
		name, method, path, contentType, host string
		status                                int
	}{
		{"simple request", http.MethodPost, "/v1/embeddings", "text/plain", "", http.StatusUnsupportedMediaType},
		{"attacker host", http.MethodPost, "/v1/embeddings", "application/json", attackerHost, http.StatusForbidden},
		{"attacker ping host", http.MethodGet, "/api/ping", "", attackerHost, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), tc.method, record.Endpoint().BaseURL()+tc.path, strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
			require.NoError(t, err)
			req.Header.Set("Content-Type", tc.contentType)
			if tc.host != "" {
				req.Host = tc.host
			}
			res, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			defer res.Body.Close()
			assert.Equal(t, tc.status, res.StatusCode)
		})
	}
}

func TestMatchesListenerHost(t *testing.T) {
	for _, tc := range []struct {
		name, authority, listener string
		matches                   bool
	}{
		{"default HTTP port", "127.0.0.1", "127.0.0.1:80", true},
		{"localhost", "LOCALHOST:9000", "127.0.0.1:9000", true},
		{"wrong port", "LOCALHOST:0", "127.0.0.1:9000", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.matches, matchesListenerHost(tc.authority, tc.listener))
		})
	}
}

type blockedListener struct {
	net.Listener
	block   *atomic.Bool
	writing chan *blockedConn
}

func (l *blockedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &blockedConn{Conn: conn, block: l.block, writing: l.writing, closed: make(chan struct{})}, nil
}

type blockedConn struct {
	net.Conn
	block   *atomic.Bool
	writing chan *blockedConn
	closed  chan struct{}
	once    sync.Once
}

func (c *blockedConn) Write(p []byte) (int, error) {
	if c.block.Load() {
		c.writing <- c
		<-c.closed
		return 0, net.ErrClosed
	}
	return c.Conn.Write(p)
}

func (c *blockedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func startServe(t *testing.T, ctx context.Context, e engine, listen func(daemon.Endpoint, context.Context) (net.Listener, error)) (daemon.RuntimeStore, daemon.RuntimeRecord, <-chan error) {
	t.Helper()
	dir := t.TempDir()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, Options{DataDir: dir}, func(context.Context, string, string) (engine, error) { return e, nil }, listen)
	}()
	store := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}
	var record daemon.RuntimeRecord
	require.Eventually(t, func() bool {
		found, _, ok, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{})
		if err != nil || !ok {
			return false
		}
		record = found
		return true
	}, 5*time.Second, 10*time.Millisecond)
	return store, record, done
}

func startEmbeddingRequest(t *testing.T, record daemon.RuntimeRecord) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, record.Endpoint().BaseURL()+"/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		done <- err
	}()
	return done
}
