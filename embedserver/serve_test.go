package embedserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
)

func TestServeDiscoveryAndCancellation(t *testing.T) {
	dir := t.TempDir()
	reader, writer, err := os.Pipe()
	require.NoError(t, err)
	stderr := os.Stderr
	os.Stderr = writer
	defer func() { os.Stderr = stderr; writer.Close(); reader.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := new(fakeEngine)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, Options{DataDir: dir}, func(context.Context, string, string) (engine, error) { return e, nil })
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
	cancel()
	require.NoError(t, <-done)
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, record.Endpoint().BaseURL()+"\n", string(printed))
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.True(t, e.closed)
}

func TestServeOwnerAndStartupFailure(t *testing.T) {
	dir := t.TempDir()
	store := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}
	release, err := store.AcquireOwnerLock(t.Context())
	require.NoError(t, err)
	held := true
	defer func() {
		if held {
			release()
		}
	}()
	called := false
	load := func(context.Context, string, string) (engine, error) {
		called = true
		return nil, errors.New("model unavailable")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, serve(ctx, Options{DataDir: dir}, load), context.Canceled)
	assert.False(t, called)
	release()
	held = false
	require.EqualError(t, serve(t.Context(), Options{DataDir: dir}, load), "model unavailable")
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestServeCancellationDrainsInference(t *testing.T) {
	dir := t.TempDir()
	entered, finished := make(chan struct{}), make(chan struct{})
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil, 0, ctx.Err()
	}}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, Options{DataDir: dir}, func(context.Context, string, string) (engine, error) { return e, nil })
	}()
	store := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}
	var record daemon.RuntimeRecord
	require.Eventually(t, func() bool {
		records, err := store.List()
		if err != nil || len(records) != 1 {
			return false
		}
		record = records[0]
		return true
	}, 5*time.Second, 10*time.Millisecond)
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, record.Endpoint().BaseURL()+"/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
		if err != nil {
			return
		}
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err == nil {
			res.Body.Close()
		}
	}()
	<-entered
	cancel()
	require.NoError(t, <-done)
	<-finished
	<-requestDone
	e.mu.Lock()
	defer e.mu.Unlock()
	assert.True(t, e.closed)
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestServeLoopbackOnly(t *testing.T) {
	require.Error(t, Serve(t.Context(), Options{DataDir: t.TempDir(), Listen: "0.0.0.0:0"}))
}

func TestMissingRuntimePreventsModelDownload(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "cache")
	_, err := loadModel(t.Context(), dir, filepath.Join(root, "missing-library"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = os.Stat(dir)
	require.ErrorIs(t, err, os.ErrNotExist)
}
