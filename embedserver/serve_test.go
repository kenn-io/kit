package embedserver

import (
	"bufio"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
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
	store, record, done := startServe(t, ctx, e)
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
	<-requestDone
	require.NoError(t, writer.Close())
	printed, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, record.Endpoint().BaseURL()+"\n", string(printed))
	records, err := store.List()
	require.NoError(t, err)
	assert.Empty(t, records)
}

func TestServeStartupFailure(t *testing.T) {
	load := func(context.Context, string, string) (engine, error) {
		return nil, errors.New("model unavailable")
	}
	require.EqualError(t, serve(t.Context(), Options{DataDir: t.TempDir()}, load), "model unavailable")
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

func TestServeCancellationInterruptsResponse(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	e := &fakeEngine{run: func(context.Context, string) ([]float32, int, error) {
		vector := make([]float32, 768)
		for i := range vector {
			vector[i] = float32(i + 1)
		}
		return vector, 7, nil
	}}
	_, record, done := startServe(t, ctx, e)
	address, err := net.ResolveTCPAddr("tcp", record.Address)
	require.NoError(t, err)
	conn, err := net.DialTCP("tcp", nil, address)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadBuffer(1024))
	require.NoError(t, conn.SetDeadline(time.Now().Add(10*time.Second)))
	// Read only the headers of a multi-megabyte response through a small
	// receive buffer, leaving the server writing to a stalled reader.
	body, err := json.Marshal(map[string]any{"model": "nomic-embed-text", "input": make([]string, 256)})
	require.NoError(t, err)
	_, err = fmt.Fprintf(conn, "POST /v1/embeddings HTTP/1.1\r\nHost: example.com\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
	require.NoError(t, err)
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer func() { conn.Close(); res.Body.Close() }()
	require.Equal(t, http.StatusOK, res.StatusCode)
	// The stalled response must not retain the inference gate.
	requestCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, record.Endpoint().BaseURL()+"/v1/embeddings", strings.NewReader(`{"model":"nomic-embed-text","input":"next"}`))
	require.NoError(t, err)
	next, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	next.Body.Close()
	assert.Equal(t, http.StatusOK, next.StatusCode)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "stalled response prevented shutdown")
	}
	require.NoError(t, conn.SetReadBuffer(1<<20))
	_, err = io.Copy(io.Discard, conn)
	assert.NotErrorIs(t, err, os.ErrDeadlineExceeded, "shutdown left the stalled connection open")
}

func startServe(t *testing.T, ctx context.Context, e engine) (daemon.RuntimeStore, daemon.RuntimeRecord, <-chan error) {
	t.Helper()
	dir := t.TempDir()
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
	return store, record, done
}
