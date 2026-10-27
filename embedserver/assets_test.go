package embedserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	for _, tc := range []struct {
		name, body string
		size       int64
	}{
		{"digest mismatch", "wrong", 5},
		{"too long", "valid", 4},
		{"too short", "valid", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model")
			require.NoError(t, os.WriteFile(path, []byte("old corrupt cache"), 0o600))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			a := asset{"model", server.URL, hex.EncodeToString(digest[:]), tc.size}
			if tc.name == "too long" {
				reader := strings.NewReader("valid trailing")
				require.Error(t, verify(reader, io.Discard, a))
				assert.Equal(t, 9, reader.Len())
			}
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
		<-r.Context().Done()
	}))
	defer func() { cancel(); server.Close() }()
	dir := t.TempDir()
	a := asset{"model", server.URL, "unused", 100}
	done := make(chan error, 1)
	go func() { done <- cacheAsset(ctx, server.Client(), dir, a) }()
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		return err == nil && len(entries) == 1 && entries[0].Name() != "model"
	}, 5*time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}
