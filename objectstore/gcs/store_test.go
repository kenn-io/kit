package gcs

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

func TestCanceledRange(t *testing.T) {
	f, s := newFixture(t, "")
	f.objects["namespace/object"] = fixtureObject{Data: []byte("abcdef"), Generation: "3"}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	body, err := s.OpenRange(ctx, "object", "3", 1, 3)
	require.NoError(t, err)
	cancel()
	_, canceledErr := io.ReadAll(body)
	require.ErrorIs(t, canceledErr, context.Canceled)
	require.NoError(t, body.Close())
}

func TestRangeAndTruncation(t *testing.T) {
	for _, mode := range []string{"", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			f, s := newFixture(t, mode)
			f.objects["namespace/object"] = fixtureObject{Data: []byte("abcdef"), Generation: "3"}
			body, err := s.OpenRange(t.Context(), "object", "3", 1, 3)
			require.NoError(t, err)
			got, err := io.ReadAll(body)
			if mode == "truncated" {
				require.ErrorIs(t, err, io.ErrUnexpectedEOF)
			} else {
				require.NoError(t, err)
				assert.Equal(t, []byte("bcd"), got)
			}
			require.NoError(t, body.Close())
		})
	}
}

func TestInvalidSourceDoesNotPublish(t *testing.T) {
	f, s := newFixture(t, "")
	receipt, err := s.Publish(t.Context(), "object", bytes.NewReader([]byte("bad")), 3, sha256.Sum256([]byte("good")))
	require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
	assert.Zero(t, receipt)
	assert.Zero(t, f.starts)
}

func TestEmptyPublication(t *testing.T) {
	f, s := newFixture(t, "")
	receipt, err := s.Publish(t.Context(), "empty", bytes.NewReader(nil), 0, sha256.Sum256(nil))
	require.NoError(t, err)
	assert.Equal(t, int64(0), receipt.Object.Size)
	assert.Contains(t, f.objects, "namespace/empty")
}

func TestCanceledBeforeFinalHTTPDispatch(t *testing.T) {
	listener, err := new(net.ListenConfig).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	observed := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer func() { _ = conn.Close() }()
			err = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err == nil {
				var header [6]byte
				_, err = io.ReadFull(conn, header[:])
			}
		}
		cancel()
		observed <- err
	}()
	f, s := newFixture(t, "")
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	s.client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPut {
			clone := r.Clone(r.Context())
			u := *r.URL
			u.Scheme = "https"
			u.Host = listener.Addr().String()
			clone.URL = &u
			return transport.RoundTrip(clone)
		}
		return transport.RoundTrip(r)
	})
	data := []byte("unpublished")
	receipt, err := s.Publish(ctx, "object", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
	require.ErrorIs(t, err, context.Canceled)
	require.NotErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	require.NoError(t, <-observed)
	assert.NotContains(t, f.objects, "namespace/object")
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
