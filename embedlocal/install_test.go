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
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/selfupdate"
)

func installFixture(t *testing.T, mode string, cancel context.CancelFunc) (*http.Client, []embedassets.Asset, *sync.Map, func()) {
	t.Helper()
	var archive bytes.Buffer
	names := append(embedassets.NativeFiles(), embedassets.ExecutableName())
	suffix := ".tar.gz"
	if runtime.GOOS == "windows" {
		suffix = ".zip"
		writer := zip.NewWriter(&archive)
		for _, name := range names {
			out, err := writer.Create(name)
			require.NoError(t, err)
			_, err = out.Write([]byte("fixture"))
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
	} else {
		compressor := gzip.NewWriter(&archive)
		writer := tar.NewWriter(compressor)
		for _, name := range names {
			require.NoError(t, writer.WriteHeader(&tar.Header{Name: name, Size: 7, Mode: 0o755}))
			_, err := writer.Write([]byte("fixture"))
			require.NoError(t, err)
		}
		require.NoError(t, writer.Close())
		require.NoError(t, compressor.Close())
	}
	name := "kennembed_" + strings.TrimPrefix(embedassets.BundleTag, "v") + "_" + runtime.GOOS + "_" + runtime.GOARCH + suffix
	digest := sha256.Sum256(archive.Bytes())
	calls := new(sync.Map)
	var interrupted atomic.Bool
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		count, _ := calls.LoadOrStore(req.URL.Path, new(atomic.Int64))
		count.(*atomic.Int64).Add(1)
		switch req.URL.Path {
		case "/repos/kenn-io/kit/releases/tags/" + embedassets.BundleTag:
			_ = json.MarshalWrite(w, selfupdate.Release{TagName: embedassets.BundleTag, Assets: []selfupdate.Asset{{Name: name, Size: int64(archive.Len()), BrowserDownloadURL: server.URL + "/archive"}, {Name: "SHA256SUMS", BrowserDownloadURL: server.URL + "/checksums"}}})
		case "/repos/kenn-io/kit/releases/latest":
			_ = json.MarshalWrite(w, selfupdate.Release{TagName: "v99.0.0"})
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%x  %s\n", digest, name)
		case "/archive":
			data := bytes.Clone(archive.Bytes())
			if mode == "digest" {
				data[len(data)-1] ^= 1
			}
			_, _ = w.Write(data)
		case "/model.onnx", "/tokenizer.json":
			if req.URL.Path == "/tokenizer.json" && mode == "canceled" && !interrupted.Swap(true) {
				cancel()
				<-req.Context().Done()
				return
			}
			_, _ = w.Write([]byte("model fixture"))
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "example.com"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)
	sum := sha256.Sum256([]byte("model fixture"))
	var assets []embedassets.Asset
	for _, name := range []string{"model.onnx", "tokenizer.json"} {
		assets = append(assets, embedassets.Asset{Name: name, URL: server.URL + "/" + name, Digest: hex.EncodeToString(sum[:]), Size: 13})
	}
	return &http.Client{Transport: transport}, assets, calls, server.Close
}

func requestCount(calls *sync.Map, path string) int64 {
	value, ok := calls.Load(path)
	if !ok {
		return 0
	}
	return value.(*atomic.Int64).Load()
}

func TestInstallRepairsSharedCopy(t *testing.T) {
	for _, repair := range []string{"missing runtime", "empty executable", "execute permission", "model only", "offline", "concurrent callers"} {
		t.Run(repair, func(t *testing.T) {
			client, assets, calls, closeServer := installFixture(t, "", nil)
			var confirmations atomic.Int64
			opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { confirmations.Add(1); return true, nil }}
			if repair == "concurrent callers" {
				var wg sync.WaitGroup
				results, errs := make([]Installation, 2), make([]error, 2)
				for i := range results {
					wg.Go(func() { results[i], errs[i] = install(t.Context(), opts, client, assets) })
				}
				wg.Wait()
				for _, err := range errs {
					require.NoError(t, err)
				}
				require.Equal(t, results[0], results[1])
				assert.Equal(t, int64(1), confirmations.Load())
				require.Equal(t, filepath.Join(opts.DataDir, "bin", embedassets.BundleTag, embedassets.ExecutableName()), results[0].Executable)
				return
			}
			result, err := install(t.Context(), opts, client, assets)
			require.NoError(t, err)
			before := requestCount(calls, "/archive")
			switch repair {
			case "missing runtime":
				require.NoError(t, os.Remove(filepath.Join(filepath.Dir(result.Executable), embedassets.LibraryName())))
			case "empty executable":
				require.NoError(t, os.Truncate(result.Executable, 0))
			case "execute permission":
				if runtime.GOOS == "windows" {
					t.Skip("Unix execute permissions")
				}
				require.NoError(t, os.Chmod(result.Executable, 0o600))
			case "model only":
				require.NoError(t, os.Remove(filepath.Join(embedassets.ModelDir(opts.DataDir), "tokenizer.json")))
			case "offline":
				closeServer()
			}
			opts.Confirm = func(_ context.Context, size int64) (bool, error) {
				confirmations.Add(1)
				if repair == "offline" {
					require.FailNow(t, "offline reuse asked for confirmation")
				}
				if repair == "model only" {
					assert.Equal(t, int64(13), size)
					assert.Equal(t, int64(1), requestCount(calls, "/tokenizer.json"))
				} else {
					assert.Equal(t, int64(bundleDownloadBytes), size)
				}
				return true, nil
			}
			again, err := install(t.Context(), opts, client, assets)
			require.NoError(t, err)
			assert.Equal(t, result, again)
			require.NoError(t, checkBundle(filepath.Dir(again.Executable)))
			expectedConfirmations := int64(2)
			if repair == "offline" {
				expectedConfirmations = 1
			}
			assert.Equal(t, expectedConfirmations, confirmations.Load())
			if repair == "model only" {
				assert.Equal(t, before, requestCount(calls, "/archive"))
			}
			if repair == "empty executable" {
				assert.Equal(t, before+1, requestCount(calls, "/archive"))
			}
		})
	}
}

func TestInstallConsentAndFailures(t *testing.T) {
	for _, mode := range []string{"missing confirmation", "declined with stale staging", "digest", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client, assets, calls, _ := installFixture(t, mode, cancel)
			opts := InstallOptions{DataDir: t.TempDir()}
			if mode == "declined with stale staging" {
				stale := filepath.Join(opts.DataDir, ".install-interrupted")
				require.NoError(t, os.Mkdir(stale, 0o700))
				require.NoError(t, os.WriteFile(filepath.Join(stale, "partial"), []byte("partial"), 0o600))
			}
			if mode != "missing confirmation" {
				opts.Confirm = func(_ context.Context, size int64) (bool, error) {
					assert.Equal(t, int64(bundleDownloadBytes+26), size)
					assert.Zero(t, requestCount(calls, "/archive"))
					return mode != "declined with stale staging", nil
				}
			}
			_, err := install(ctx, opts, client, assets)
			require.Error(t, err)
			if mode == "canceled" {
				require.ErrorIs(t, err, context.Canceled)
				require.NoError(t, checkBundle(filepath.Join(opts.DataDir, "bin", embedassets.BundleTag)))
			} else {
				assert.NoDirExists(t, filepath.Join(opts.DataDir, "bin", embedassets.BundleTag))
			}
			if mode == "digest" {
				require.EqualError(t, err, "download kennembed: asset size or SHA-256 mismatch")
			}
			leftovers, err := filepath.Glob(filepath.Join(opts.DataDir, ".install-*"))
			require.NoError(t, err)
			assert.Empty(t, leftovers)
			if mode == "missing confirmation" || mode == "declined with stale staging" {
				calls.Range(func(_, value any) bool { assert.Zero(t, value.(*atomic.Int64).Load()); return true })
			}
			if mode == "canceled" {
				opts.Confirm = func(_ context.Context, size int64) (bool, error) { assert.Equal(t, int64(13), size); return true, nil }
				_, err = install(t.Context(), opts, client, assets)
				require.NoError(t, err)
				assert.Equal(t, int64(1), requestCount(calls, "/model.onnx"))
				assert.Equal(t, int64(1), requestCount(calls, "/archive"))
			}
		})
	}
}
