package embedserver

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
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
	store, record, done := startServe(t, ctx, t.TempDir(), e, daemon.Endpoint.ListenContext)
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
		{"non-loopback", "0.0.0.0:0", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			err := serve(t.Context(), Options{DataDir: t.TempDir(), Listen: tc.listen}, func(context.Context, string, string) (engine, error) {
				called = true
				return nil, errors.New("model unavailable")
			}, daemon.Endpoint.ListenContext)
			require.Error(t, err)
			if tc.message != "" {
				require.ErrorContains(t, err, tc.message)
			}
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
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, ctx.Err(), "owner lock wait exceeded its startup timeout")
	assert.False(t, loaded)
}

func TestServeRemovesStaleRecords(t *testing.T) {
	const stalePID = 999999
	if daemon.ProcessAlive(stalePID) {
		t.Skipf("pid %d is alive on this host", stalePID)
	}
	dir := t.TempDir()
	stale, err := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}.Write(daemon.RuntimeRecord{PID: stalePID, Network: daemon.NetworkTCP, Address: "127.0.0.1:9"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	store, record, done := startServe(t, ctx, dir, new(fakeEngine), daemon.Endpoint.ListenContext)
	records, err := store.List()
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, record.PID, records[0].PID)
	assert.NoFileExists(t, stale)
	cancel()
	require.NoError(t, <-done)
}

func startServe(t *testing.T, ctx context.Context, dir string, e engine, listen func(daemon.Endpoint, context.Context) (net.Listener, error)) (daemon.RuntimeStore, daemon.RuntimeRecord, <-chan error) {
	t.Helper()
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

func TestServePrintsFailureBeforeOwnerRelease(t *testing.T) {
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	defer reader.Close()
	stderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = stderr }()
	store := daemon.RuntimeStore{Dir: t.TempDir(), Prefix: "kit-embed"}
	// Hold the diagnostic write open while checking the actual server's owner lock.
	failure := errors.New("model unavailable: " + strings.Repeat("x", 1<<20))
	done := make(chan error, 1)
	go func() {
		err := serve(t.Context(), Options{DataDir: store.Dir}, func(context.Context, string, string) (engine, error) { return nil, failure }, daemon.Endpoint.ListenContext)
		// The CLI prints returned errors after Serve returns.
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
		}
		writer.Close()
		done <- err
	}()
	defer func() {
		_, err := io.Copy(io.Discard, reader)
		require.NoError(t, err)
		require.ErrorIs(t, <-done, failure)
	}()
	output := bufio.NewReader(reader)
	loading, err := output.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "Loading nomic-embed-text model...\n", loading)
	prefix := make([]byte, len("model unavailable:"))
	_, err = io.ReadFull(output, prefix)
	require.NoError(t, err)
	require.Equal(t, "model unavailable:", string(prefix))
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond) //nolint:kennlint // probes the OS lock while the stderr pipe holds the diagnostic write open
	defer cancel()
	release, err := store.AcquireOwnerLock(ctx)
	if release != nil {
		release()
	}
	require.ErrorIs(t, err, context.DeadlineExceeded, "failure must be visible before ownership is released")
}
