package embedserver

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
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
	entered, finished := make(chan struct{}), make(chan struct{})
	closedAfterInference := false
	e := &fakeEngine{run: func(ctx context.Context, _ string) ([]float32, int, error) {
		close(entered)
		<-ctx.Done()
		close(finished)
		return nil, 0, ctx.Err()
	}, onClose: func() {
		select {
		case <-finished:
			closedAfterInference = true
		default:
		}
	}}
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
	requestCtx, stopRequest := context.WithCancel(t.Context())
	defer stopRequest()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, record.Endpoint().BaseURL()+"/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"x"}`))
	require.NoError(t, err)
	requestDone := make(chan error, 1)
	go func() {
		res, err := http.DefaultClient.Do(req)
		if err == nil {
			res.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "inference did not start")
	}
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancellation did not drain inference")
	}
	assert.True(t, closedAfterInference)
	require.NoError(t, <-requestDone)
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, record.Endpoint().BaseURL()+"\n", string(printed))
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
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
}

func TestServeLoopbackOnly(t *testing.T) {
	called := false
	err := serve(t.Context(), Options{DataDir: t.TempDir(), Listen: "0.0.0.0:0"}, func(context.Context, string, string) (engine, error) {
		called = true
		return nil, errors.New("loader reached")
	})
	require.ErrorContains(t, err, "not loopback")
	assert.False(t, called)
}
