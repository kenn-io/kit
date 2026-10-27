package embedserver

import (
	"context"
	"errors"
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

func TestServeDiscoveryAndStop(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := new(fakeEngine)
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, Options{DataDir: dir}, func(context.Context, string, string) (engine, error) { return e, nil })
	}()
	store := daemon.RuntimeStore{Dir: dir, Prefix: "kit-embed"}
	var record daemon.RuntimeRecord
	var proof *daemon.Proof
	require.Eventually(t, func() bool {
		var err error
		proof, err = LoadProof(dir)
		if err != nil {
			return false
		}
		records, err := store.List()
		if err != nil || len(records) != 1 {
			return false
		}
		record = records[0]
		_, err = proof.Probe(t.Context(), record, daemon.ProbeOptions{})
		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	discovered, _, found, err := daemon.Discover(t.Context(), store, daemon.DiscoverOptions{Proof: proof})
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, record.Address, discovered.Address)
	wrong, err := daemon.NewProof([]byte("different proof"))
	require.NoError(t, err)
	_, err = wrong.Probe(t.Context(), record, daemon.ProbeOptions{})
	require.Error(t, err)
	key, err := os.Stat(filepath.Join(dir, "proof.key"))
	require.NoError(t, err)
	assert.NotZero(t, key.Size())
	err = daemon.Stop(t.Context(), record, daemon.StopOptions{Proof: proof, Request: func(ctx context.Context, r daemon.RuntimeRecord) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Endpoint().BaseURL()+"/api/shutdown", nil)
		if err != nil {
			return err
		}
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		assert.Equal(t, http.StatusAccepted, res.StatusCode)
		return nil
	}})
	require.NoError(t, err)
	require.NoError(t, <-done)
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
	_, err = os.Stat(filepath.Join(dir, "proof.key"))
	require.ErrorIs(t, err, os.ErrNotExist)
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
