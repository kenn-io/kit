package embedserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCacheVerifiedAndOffline(t *testing.T) {
	data := "public model fixture"
	digest := sha256.Sum256([]byte(data))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(data)) }))
	a := asset{"model", server.URL, hex.EncodeToString(digest[:]), int64(len(data))}
	dir := t.TempDir()
	require.NoError(t, cacheAsset(t.Context(), server.Client(), dir, a))
	server.Close()
	require.NoError(t, cacheAsset(t.Context(), server.Client(), dir, a))
	got, err := os.ReadFile(filepath.Join(dir, "model"))
	require.NoError(t, err)
	assert.Equal(t, data, string(got))
}

func TestCacheRejectsBadDownloads(t *testing.T) {
	digest := sha256.Sum256([]byte("valid"))
	for _, body := range []string{"wrong", "short data", "va"} {
		t.Run(body, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model")
			require.NoError(t, os.WriteFile(path, []byte("old corrupt cache"), 0o600))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
			defer server.Close()
			a := asset{"model", server.URL, hex.EncodeToString(digest[:]), 5}
			require.Error(t, cacheAsset(t.Context(), server.Client(), dir, a))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "old corrupt cache", string(got))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		})
	}
}

func TestCacheInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		cancel()
		<-r.Context().Done()
	}))
	defer server.Close()
	dir := t.TempDir()
	a := asset{"model", server.URL, "unused", 100}
	require.Error(t, cacheAsset(ctx, server.Client(), dir, a))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
