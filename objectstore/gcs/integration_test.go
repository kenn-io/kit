//go:build gcslive

package gcs

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/objectstore"
)

// TestGCSLiveContract requires separate operator authorization. It never loads
// ambient credentials and is skipped unless every setting is supplied explicitly.
func TestGCSLiveContract(t *testing.T) {
	if os.Getenv("KIT_GCS_TEST_LIVE") != "1" {
		t.Skip("requires separately authorized disposable GCS namespace")
	}
	names := []string{"KIT_GCS_TEST_BUCKET", "KIT_GCS_TEST_PREFIX", "KIT_GCS_TEST_KEY_ID", "KIT_GCS_TEST_SECRET", "KIT_GCS_TEST_ENDPOINT"}
	values := map[string]string{}
	for _, name := range names {
		value := os.Getenv(name)
		require.NotEmpty(t, value, "explicit %s is required", name)
		values[name] = value
	}
	var nonce [12]byte
	_, err := rand.Read(nonce[:])
	require.NoError(t, err)
	cfg := Config{Bucket: values[names[0]], Prefix: values[names[1]] + "/" + hex.EncodeToString(nonce[:]), KeyID: values[names[2]], Secret: values[names[3]], Endpoint: values[names[4]]}
	s, err := New(cfg)
	require.NoError(t, err)
	cleanupCtx := t.Context()
	for _, item := range []struct {
		key  string
		size int
	}{{"empty", 0}, {"small", 17}, {"resumable", (8 << 20) + 19}} {
		t.Run(item.key, func(t *testing.T) {
			data := bytes.Repeat([]byte("x"), item.size)
			digest := sha256.Sum256(data)
			receipt, err := s.Publish(t.Context(), item.key, bytes.NewReader(data), int64(len(data)), digest)
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, s.Delete(cleanupCtx, item.key, receipt.Object.Revision)) })
			assert.Equal(t, "provider-crc32c", receipt.Verification)
			again, err := s.Publish(t.Context(), item.key, bytes.NewReader(data), int64(len(data)), digest)
			require.NoError(t, err)
			assert.Equal(t, receipt.Object.Revision, again.Object.Revision)
			assert.Equal(t, "readback-sha256", again.Verification)
			other := []byte("different")
			zero, err := s.Publish(t.Context(), item.key, bytes.NewReader(other), int64(len(other)), sha256.Sum256(other))
			require.ErrorIs(t, err, objectstore.ErrExists)
			assert.Zero(t, zero)
			zero, err = s.Publish(t.Context(), "invalid-source", bytes.NewReader(data), int64(len(data)), sha256.Sum256([]byte("invalid")))
			require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
			assert.Zero(t, zero)
			if item.size > 0 {
				body, err := s.OpenRange(t.Context(), item.key, receipt.Object.Revision, 0, 3)
				require.NoError(t, err)
				got, err := io.ReadAll(body)
				require.NoError(t, err)
				require.NoError(t, body.Close())
				assert.Equal(t, data[:3], got)
				_, err = s.OpenRange(t.Context(), item.key, "0", 0, 1)
				require.ErrorIs(t, err, objectstore.ErrRevisionChanged)
			}
			require.ErrorIs(t, s.Delete(t.Context(), item.key, "0"), objectstore.ErrRevisionChanged)
		})
	}
	keys := []string{"inventory/a", "inventory/b"}
	for _, key := range keys {
		data := []byte(key)
		receipt, err := s.Publish(t.Context(), key, bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
		require.NoError(t, err)
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(cleanupCtx), 30*time.Second)
			defer cancel()
			assert.NoError(t, s.Delete(ctx, key, receipt.Object.Revision))
		})
	}
	first, err := s.ListPage(t.Context(), "inventory/", "", 1)
	require.NoError(t, err)
	require.Len(t, first.Objects, 1)
	require.NotEmpty(t, first.NextToken)
	second, err := s.ListPage(t.Context(), "inventory/", first.NextToken, 1)
	require.NoError(t, err)
	require.Len(t, second.Objects, 1)
	assert.Equal(t, "inventory/a", first.Objects[0].Key)
	assert.Equal(t, "inventory/b", second.Objects[0].Key)
	assert.Empty(t, second.NextToken)
	t.Run("lost-completion", func(t *testing.T) {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		defer transport.CloseIdleConnections()
		lost := false
		cfg.Client = &http.Client{Timeout: 2 * time.Minute, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			response, err := transport.RoundTrip(r)
			if err == nil && !lost && r.Method == http.MethodPut && strings.HasPrefix(r.Header.Get("Content-Range"), "bytes ") && response.StatusCode/100 == 2 {
				lost = true
				_ = response.Body.Close()
				return nil, io.ErrUnexpectedEOF
			}
			return response, err
		})}
		recovering, err := New(cfg)
		require.NoError(t, err)
		data := []byte("persisted before the response was discarded")
		receipt, err := recovering.Publish(t.Context(), "lost-completion", bytes.NewReader(data), int64(len(data)), sha256.Sum256(data))
		require.NoError(t, err)
		t.Cleanup(func() { assert.NoError(t, s.Delete(cleanupCtx, "lost-completion", receipt.Object.Revision)) })
		assert.True(t, lost)
		assert.Equal(t, "readback-sha256", receipt.Verification)
	})
	require.NoError(t, s.Delete(t.Context(), "absent", ""))
}
