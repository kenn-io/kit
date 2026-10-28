package embedlocal

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/selfupdate"
)

// The model boundary uses a tiny verified HTTP asset in place of the 274 MB model.
func installFixture(t *testing.T, suffix, bodyOverride string) (selfupdate.Client, func(context.Context, string, bool) error, *atomic.Int64, func()) {
	t.Helper()
	files := map[string]string{"kennembed": "fixture executable", "libonnxruntime.so.1.23.2": "runtime", "libonnxruntime.1.23.2.dylib": "runtime", "kennembed.exe": "fixture executable", "onnxruntime.dll": "runtime", "msvcp140.dll": "runtime", "msvcp140_1.dll": "runtime", "vcruntime140.dll": "runtime", "vcruntime140_1.dll": "runtime"}
	var archive bytes.Buffer
	if suffix == ".zip" {
		writer := zip.NewWriter(&archive)
		for name, content := range files {
			header := &zip.FileHeader{Name: name, Method: zip.Deflate}
			header.SetMode(0o755)
			out, err := writer.CreateHeader(header)
			require.NoError(t, err)
			_, err = out.Write([]byte(content))
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
	} else {
		compressor := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressor)
		for name, content := range files {
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Size: int64(len(content)), Mode: 0o755}))
			_, err := writer.Write([]byte(content))
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		require.NoError(t, compressor.Close())
	}
	name := "kennembed_1.2.3_" + runtime.GOOS + "_" + runtime.GOARCH + suffix
	digest := sha256.Sum256(archive.Bytes())
	var requests atomic.Int64
	var downgraded atomic.Bool
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) { downgraded.Store(true) }))
	t.Cleanup(downstream.Close)
	t.Cleanup(func() { assert.False(t, downgraded.Load(), "download followed a non-HTTPS redirect") })
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		switch req.URL.Path {
		case "/manifest":
			_ = json.MarshalWrite(w, selfupdate.Release{TagName: "v1.2.3", Assets: []selfupdate.Asset{{Name: name, Size: int64(archive.Len()), BrowserDownloadURL: server.URL + "/archive"}, {Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/checksums"}}})
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%x  %s\n", digest, name)
		case "/archive":
			if bodyOverride == "redirect" {
				http.Redirect(w, req, downstream.URL, http.StatusFound)
				return
			}
			if bodyOverride != "" {
				if bodyOverride == "wrong digest" {
					bad := bytes.Clone(archive.Bytes())
					bad[len(bad)-1] ^= 1
					_, _ = w.Write(bad)
				} else {
					_, _ = w.Write([]byte(bodyOverride))
				}
			} else {
				_, _ = w.Write(archive.Bytes())
			}
		case "/model", "/tokenizer":
			_, _ = w.Write([]byte("model fixture"))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	model := func(ctx context.Context, dir string, download bool) error {
		sum := sha256.Sum256([]byte("model fixture"))
		for _, name := range []string{"model", "tokenizer"} {
			if download {
				if err := embedassets.CacheAsset(ctx, server.Client(), dir, embedassets.Asset{Name: name, URL: server.URL + "/" + name, Digest: hex.EncodeToString(sum[:]), Size: 13}); err != nil {
					return err
				}
			} else {
				data, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil {
					return err
				}
				if sha256.Sum256(data) != sum {
					return errors.New("invalid model cache")
				}
			}
		}
		return nil
	}
	client := selfupdate.Client{
		Owner: "example", Repo: "example", BinaryName: "kennembed",
		HTTPClient: server.Client(), ReleaseManifestURL: server.URL + "/manifest", AllowUnsignedChecksums: true,
		AssetName: func(req selfupdate.AssetRequest) string { return name },
	}
	return client, model, &requests, server.Close
}

func TestInstallVerifiedOfflineAndConcurrent(t *testing.T) {
	for _, suffix := range []string{".tar.gz", ".zip"} {
		t.Run(suffix, func(t *testing.T) {
			client, model, requests, closeServer := installFixture(t, suffix, "")
			dir := t.TempDir()
			var confirmations atomic.Int64
			options := InstallOptions{DataDir: dir, Confirm: func(ctx context.Context, size int64) (bool, error) {
				assert.Zero(t, requests.Load())
				assert.Equal(t, int64(290_000_000), size)
				confirmations.Add(1)
				return true, nil
			}}
			var wg sync.WaitGroup
			results := make([]Installation, 2)
			errs := make([]error, 2)
			for i := range results {
				wg.Go(func() { results[i], errs[i] = install(t.Context(), options, client, model) })
			}
			wg.Wait()
			for i := range errs {
				require.NoError(t, errs[i])
			}
			assert.Equal(t, int64(1), confirmations.Load())
			assert.Equal(t, results[0], results[1])
			assert.Equal(t, dir, results[0].DataDir)
			assert.True(t, filepath.IsAbs(results[0].Executable))
			content, err := os.ReadFile(results[0].Executable)
			require.NoError(t, err)
			assert.Equal(t, "fixture executable", string(content))
			closeServer()
			count := requests.Load()
			options.Confirm = nil
			again, err := install(t.Context(), options, client, model)
			require.NoError(t, err)
			assert.Equal(t, results[0], again)
			assert.Equal(t, count, requests.Load())
		})
	}
}

func TestInstallDeclinedOrCanceled(t *testing.T) {
	client, model, requests, _ := installFixture(t, ".tar.gz", "")
	for _, tc := range []struct {
		name    string
		confirm func(context.Context, int64) (bool, error)
	}{
		{"missing", nil},
		{"declined", func(context.Context, int64) (bool, error) { return false, nil }},
		{"callback error", func(context.Context, int64) (bool, error) { return false, context.Canceled }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := install(t.Context(), InstallOptions{DataDir: dir, Confirm: tc.confirm}, client, model)
			require.Error(t, err)
			assert.NoDirExists(t, filepath.Join(dir, "bin"))
		})
	}
	assert.Zero(t, requests.Load())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := install(ctx, InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { return true, nil }}, client, model)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, requests.Load())
}

func TestInstallRejectsBadOrTruncatedBundle(t *testing.T) {
	for _, body := range []string{"wrong digest", "short", "redirect"} {
		t.Run(body, func(t *testing.T) {
			client, model, _, _ := installFixture(t, ".tar.gz", body)
			dir := t.TempDir()
			_, err := install(t.Context(), InstallOptions{DataDir: dir, Confirm: func(context.Context, int64) (bool, error) { return true, nil }}, client, model)
			require.Error(t, err)
			assert.NoDirExists(t, filepath.Join(dir, "bin"))
			entries, err := filepath.Glob(filepath.Join(dir, ".install-*"))
			require.NoError(t, err)
			assert.Empty(t, entries)
		})
	}
}

func TestInstallCanceledDuringModelDownload(t *testing.T) {
	client, fixtureModel, _, _ := installFixture(t, ".tar.gz", "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	model := func(ctx context.Context, dir string, download bool) error {
		if download {
			cancel()
			return ctx.Err()
		}
		return fixtureModel(ctx, dir, false)
	}
	dir := t.TempDir()
	_, err := install(ctx, InstallOptions{DataDir: dir, Confirm: func(context.Context, int64) (bool, error) { return true, nil }}, client, model)
	require.ErrorIs(t, err, context.Canceled)
	assert.NoDirExists(t, filepath.Join(dir, "bin"))
	entries, err := filepath.Glob(filepath.Join(dir, ".install-*"))
	require.NoError(t, err)
	assert.Empty(t, entries)
}
