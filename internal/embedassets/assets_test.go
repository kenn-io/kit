package embedassets

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
	a := Asset{"model", server.URL, hex.EncodeToString(digest[:]), int64(len(data))}
	dir := t.TempDir()
	require.NoError(t, CacheAsset(t.Context(), server.Client(), dir, a))
	server.Close()
	require.NoError(t, CacheAsset(t.Context(), server.Client(), dir, a))
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
		{"too short", "valid", 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "model")
			require.NoError(t, os.WriteFile(path, []byte("old corrupt cache"), 0o600))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			a := Asset{"model", server.URL, hex.EncodeToString(digest[:]), tc.size}
			require.Error(t, CacheAsset(t.Context(), server.Client(), dir, a))
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, "old corrupt cache", string(got))
			entries, err := os.ReadDir(dir)
			require.NoError(t, err)
			assert.Len(t, entries, 1)
		})
	}
}

func TestVerifyStopsAfterSizeLimit(t *testing.T) {
	reader := strings.NewReader("valid trailing")
	digest := sha256.Sum256([]byte("vali"))
	require.Error(t, verify(reader, io.Discard, Asset{Size: 4, Digest: hex.EncodeToString(digest[:])}))
	assert.Equal(t, 9, reader.Len())
}

func TestCacheInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		// Abort so a completed partial response cannot race with cancellation.
		panic(http.ErrAbortHandler)
	}))
	defer func() { cancel(); server.CloseClientConnections(); server.Close() }()
	dir := t.TempDir()
	a := Asset{"model", server.URL, "unused", 100}
	done := make(chan error, 1)
	go func() { done <- CacheAsset(ctx, server.Client(), dir, a) }()
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(dir)
		return err == nil && len(entries) == 1 && entries[0].Name() != "model"
	}, 5*time.Second, time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "cancellation did not interrupt the download")
	}
}

func TestMissingModel(t *testing.T) {
	digest := sha256.Sum256([]byte("valid"))
	for _, tc := range []struct {
		name, data             string
		directory, absent      bool
		verifyContent, missing bool
	}{
		{name: "wrong bytes verified", data: "wrong", verifyContent: true, missing: true},
		{name: "wrong bytes metadata", data: "wrong"},
		{name: "short model", data: "tiny", missing: true},
		{name: "model directory", directory: true, missing: true},
		{name: "missing file", absent: true, missing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			asset := Asset{Name: "model.onnx", Digest: hex.EncodeToString(digest[:]), Size: 5}
			path := filepath.Join(dir, asset.Name)
			if tc.directory {
				require.NoError(t, os.Mkdir(path, 0o700))
				info, err := os.Stat(path)
				require.NoError(t, err)
				asset.Size = info.Size()
			} else if !tc.absent {
				require.NoError(t, os.WriteFile(path, []byte(tc.data), 0o600))
			}
			missing, err := MissingModel(dir, []Asset{asset}, tc.verifyContent)
			require.NoError(t, err)
			if tc.missing {
				assert.Equal(t, []Asset{asset}, missing)
			} else {
				assert.Empty(t, missing)
			}
			if !tc.directory && !tc.absent {
				assert.FileExists(t, path)
			}
		})
	}
}

func TestModelPinsDiscoveryAndLoading(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "nomic-embed-text-v1.5", revision)
	assert.Equal(t, dir, ModelDir(root))
	assets := ModelAssets()
	require.Len(t, assets, 2)
	assert.Equal(t, "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/"+revision+"/onnx/model_fp16.onnx", assets[0].URL)
	assert.Equal(t, "https://huggingface.co/nomic-ai/nomic-embed-text-v1.5/resolve/"+revision+"/tokenizer.json", assets[1].URL)

	previous := modelAssets
	digest := sha256.Sum256([]byte("valid"))
	modelAssets = []Asset{{Name: "model.onnx", Digest: hex.EncodeToString(digest[:]), Size: 5}}
	t.Cleanup(func() { modelAssets = previous })
	require.NoError(t, os.MkdirAll(dir, 0o700))
	path := filepath.Join(dir, "model.onnx")
	require.NoError(t, os.WriteFile(path, []byte("bad"), 0o600))
	assert.False(t, ModelAvailable(root, TextModel))
	require.NoError(t, os.WriteFile(path, []byte("wrong"), 0o600))
	assert.True(t, ModelAvailable(root, TextModel), "discovery checks completeness without hashing")
	_, err := InstalledModel(root, TextModel)
	require.ErrorContains(t, err, "missing or corrupt model.onnx")
	require.NoError(t, os.WriteFile(path, []byte("valid"), 0o600))
	loaded, err := InstalledModel(root, TextModel)
	require.NoError(t, err)
	assert.Equal(t, dir, loaded)
}
