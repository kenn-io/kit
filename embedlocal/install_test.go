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
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/internal/embedassets"
	"go.kenn.io/kit/selfupdate"
)

// modelFixture spans several network reads so progress arrives within a file.
var modelFixture = bytes.Repeat([]byte("model fixture "), 5000)

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
			_, _ = w.Write(archive.Bytes())
		case "/model.onnx", "/tokenizer.json", "/glintr100.onnx", "/yunet.onnx":
			if req.URL.Path == "/tokenizer.json" && mode == "canceled" && !interrupted.Swap(true) {
				cancel()
				<-req.Context().Done()
				// Abort so a successful empty response cannot race with cancellation.
				panic(http.ErrAbortHandler)
			}
			_, _ = w.Write(modelFixture)
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
	sum := sha256.Sum256(modelFixture)
	var assets []embedassets.Asset
	modelNames := []string{"model.onnx", "tokenizer.json"}
	if mode == "faces" {
		modelNames = []string{"glintr100.onnx", "yunet.onnx"}
	}
	for _, name := range modelNames {
		assets = append(assets, embedassets.Asset{Name: name, URL: server.URL + "/" + name, Digest: hex.EncodeToString(sum[:]), Size: int64(len(modelFixture))})
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

func TestInstallConcurrentCallers(t *testing.T) {
	client, assets, _, _ := installFixture(t, "", nil)
	var confirmations atomic.Int64
	opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { confirmations.Add(1); return true, nil }}
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
}

func TestCheckBundle(t *testing.T) {
	for _, damage := range []string{"missing runtime", "empty executable", "execute permission"} {
		t.Run(damage, func(t *testing.T) {
			dir := t.TempDir()
			for _, name := range append(embedassets.NativeFiles(), embedassets.ExecutableName()) {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("fixture"), 0o755))
			}
			switch damage {
			case "missing runtime":
				require.NoError(t, os.Remove(filepath.Join(dir, embedassets.LibraryName())))
			case "empty executable":
				require.NoError(t, os.Truncate(filepath.Join(dir, embedassets.ExecutableName()), 0))
			case "execute permission":
				if runtime.GOOS == "windows" {
					t.Skip("Unix execute permissions")
				}
				require.NoError(t, os.Chmod(filepath.Join(dir, embedassets.ExecutableName()), 0o600))
			}
			require.Error(t, checkBundle(dir))
		})
	}
}

func TestInstallRepairsSharedCopy(t *testing.T) {
	for _, repair := range []string{"missing runtime", "model only", "offline"} {
		t.Run(repair, func(t *testing.T) {
			client, assets, calls, closeServer := installFixture(t, "", nil)
			var confirmations atomic.Int64
			opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { confirmations.Add(1); return true, nil }}
			result, err := install(t.Context(), opts, client, assets)
			require.NoError(t, err)
			before := requestCount(calls, "/archive")
			switch repair {
			case "missing runtime":
				require.NoError(t, os.Remove(filepath.Join(filepath.Dir(result.Executable), embedassets.LibraryName())))
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
					assert.Equal(t, int64(70_000), size)
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
				assert.FileExists(t, filepath.Join(embedassets.ModelDir(opts.DataDir), "tokenizer.json"))
				assert.Equal(t, int64(2), requestCount(calls, "/tokenizer.json"))
			}
			if repair == "missing runtime" {
				assert.Equal(t, before+1, requestCount(calls, "/archive"))
			}
		})
	}
}

func TestInstallConsentAndFailures(t *testing.T) {
	for _, mode := range []string{"missing confirmation", "declined"} {
		t.Run(mode, func(t *testing.T) {
			client, assets, calls, _ := installFixture(t, mode, nil)
			opts := InstallOptions{DataDir: t.TempDir()}
			if mode == "declined" {
				opts.Confirm = func(_ context.Context, size int64) (bool, error) {
					assert.Equal(t, int64(bundleDownloadBytes+140_000), size)
					return false, nil
				}
			}
			_, err := install(t.Context(), opts, client, assets)
			require.Error(t, err)
			assert.NoDirExists(t, filepath.Join(opts.DataDir, "bin", embedassets.BundleTag))
			calls.Range(func(_, value any) bool { assert.Zero(t, value.(*atomic.Int64).Load()); return true })
		})
	}
}

func TestInstallCanceledDownloads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	client, assets, calls, _ := installFixture(t, "canceled", cancel)
	opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { return true, nil }}
	stale := filepath.Join(opts.DataDir, ".install-interrupted")
	require.NoError(t, os.Mkdir(stale, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(stale, "partial"), []byte("partial"), 0o600))
	_, err := install(ctx, opts, client, assets)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, checkBundle(filepath.Join(opts.DataDir, "bin", embedassets.BundleTag)))
	leftovers, err := filepath.Glob(filepath.Join(opts.DataDir, ".install-*"))
	require.NoError(t, err)
	assert.Empty(t, leftovers)
	_, err = install(t.Context(), opts, client, assets)
	require.NoError(t, err)
	assert.Equal(t, int64(1), requestCount(calls, "/model.onnx"))
}

func TestInstallReportsProgress(t *testing.T) {
	for _, kind := range []string{"fresh", "model only"} {
		t.Run(kind, func(t *testing.T) {
			client, assets, _, _ := installFixture(t, "", nil)
			opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { return true, nil }}
			if kind == "model only" {
				_, err := install(t.Context(), opts, client, assets)
				require.NoError(t, err)
				require.NoError(t, os.Remove(filepath.Join(embedassets.ModelDir(opts.DataDir), "tokenizer.json")))
			}
			type update struct{ downloaded, total int64 }
			var updates []update
			opts.Progress = func(downloaded, total int64) { updates = append(updates, update{downloaded, total}) }
			_, err := install(t.Context(), opts, client, assets)
			require.NoError(t, err)

			require.NotEmpty(t, updates)
			total := updates[0].total
			assert.Equal(t, update{0, total}, updates[0])
			assert.Equal(t, update{total, total}, updates[len(updates)-1], "the total must equal the bytes downloaded, so it uses the release size of the bundle")
			if kind == "model only" {
				assert.Equal(t, int64(70_000), total)
			} else {
				assert.Greater(t, total, int64(140_000), "a fresh install counts the bundle")
			}
			for i := 1; i < len(updates); i++ {
				assert.Equal(t, total, updates[i].total)
				assert.Greater(t, updates[i].downloaded, updates[i-1].downloaded)
			}
			assert.True(t, slices.ContainsFunc(updates, func(u update) bool { return u.downloaded > total-70_000 && u.downloaded < total }),
				"progress must arrive while the last file downloads, not only when it finishes")
		})
	}
}

func TestInstallFaceSelection(t *testing.T) {
	client, assets, _, _ := installFixture(t, "faces", nil)
	opts := InstallOptions{DataDir: t.TempDir(), Model: FaceModel, Confirm: func(context.Context, int64) (bool, error) { return true, nil }}
	_, err := install(t.Context(), opts, client, assets)
	require.NoError(t, err)
	faceDir, _, err := embedassets.SelectedModel(opts.DataDir, FaceModel)
	require.NoError(t, err)
	entries, err := os.ReadDir(faceDir)
	require.NoError(t, err)
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	assert.Equal(t, []string{"glintr100.onnx", "yunet.onnx"}, names)
	assert.NoDirExists(t, embedassets.ModelDir(opts.DataDir))
}

func TestInstallFaceConsentSize(t *testing.T) {
	_, assets, err := embedassets.SelectedModel(t.TempDir(), FaceModel)
	require.NoError(t, err)
	var expected int64
	for _, asset := range assets {
		expected += asset.Size
	}
	_, err = Install(t.Context(), InstallOptions{DataDir: t.TempDir(), Model: FaceModel, Confirm: func(_ context.Context, size int64) (bool, error) {
		assert.Equal(t, expected+bundleDownloadBytes, size)
		return false, nil
	}})
	require.ErrorContains(t, err, "declined")
}

func TestInstallReusesNewestTextBundle(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		versions   []string
		broken     bool
		wantError  bool
	}{
		{name: "newest", versions: []string{"v0.46.0", "v0.47.0"}},
		{name: "numeric order", versions: []string{"v0.9.0", "v0.10.0"}},
		{name: "current ready", versions: []string{"v0.46.0", embedassets.BundleTag}},
		{name: "current broken", versions: []string{"v0.46.0", embedassets.BundleTag}, broken: true, wantError: true},
		{name: "faces require current", mode: "faces", versions: []string{"v0.46.0", "v0.47.0"}, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, assets, calls, closeServer := installFixture(t, tc.mode, nil)
			opts := InstallOptions{DataDir: t.TempDir(), Confirm: func(context.Context, int64) (bool, error) { return true, nil }}
			if tc.mode == "faces" {
				opts.Model = FaceModel
			}
			current, err := install(t.Context(), opts, client, assets)
			require.NoError(t, err)
			require.NoError(t, os.Rename(filepath.Dir(current.Executable), filepath.Join(opts.DataDir, "bin", tc.versions[0])))
			bundle := filepath.Join(opts.DataDir, "bin", tc.versions[1])
			require.NoError(t, os.MkdirAll(bundle, 0o700))
			for _, name := range append(embedassets.NativeFiles(), embedassets.ExecutableName()) {
				if tc.broken && name == embedassets.LibraryName() {
					continue
				}
				require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("fixture"), 0o755))
			}
			closeServer()
			opts.Confirm = nil
			reused, err := install(t.Context(), opts, client, assets)
			if tc.wantError {
				require.ErrorContains(t, err, "provide Confirm")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, filepath.Join(bundle, embedassets.ExecutableName()), reused.Executable)
			assert.Equal(t, int64(1), requestCount(calls, "/archive"))
		})
	}
}
