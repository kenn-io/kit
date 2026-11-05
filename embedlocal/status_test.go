package embedlocal

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
)

func TestInspectFiles(t *testing.T) {
	for _, tc := range []struct {
		name          string
		bundle, model bool
		invalid       string
		bytes         int64
	}{
		{name: "missing directory", bytes: 294570424},
		{name: "complete", bundle: true, model: true},
		{name: "model missing", bundle: true, bytes: 274570424},
		{name: "bundle missing", model: true, bytes: 20000000},
		{name: "short model", bundle: true, model: true, invalid: "short", bytes: 273859028},
		{name: "empty model", bundle: true, model: true, invalid: "empty", bytes: 273859028},
		{name: "model directory", bundle: true, model: true, invalid: "directory", bytes: 273859028},
		{name: "empty bundle", bundle: true, model: true, invalid: "bundle", bytes: 20000000},
		{name: "nonexecutable bundle", bundle: true, model: true, invalid: "permission", bytes: 20000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.invalid == "permission" && runtime.GOOS == "windows" {
				t.Skip("Windows does not use executable mode bits")
			}
			dir := filepath.Join(t.TempDir(), "shared")
			if tc.bundle {
				bundle := filepath.Join(dir, "bin", embedassets.BundleTag)
				require.NoError(t, os.MkdirAll(bundle, 0o700))
				for _, name := range append(embedassets.NativeFiles(), embedassets.ExecutableName()) {
					require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("fixture"), 0o700))
				}
				binary := filepath.Join(bundle, embedassets.ExecutableName())
				if tc.invalid == "bundle" {
					require.NoError(t, os.Truncate(binary, 0))
				}
				if tc.invalid == "permission" {
					require.NoError(t, os.Chmod(binary, 0o600))
				}
			}
			if tc.model {
				model := embedassets.ModelDir(dir)
				require.NoError(t, os.MkdirAll(model, 0o700))
				for _, a := range embedassets.ModelAssets() {
					f, err := os.Create(filepath.Join(model, a.Name))
					require.NoError(t, err)
					require.NoError(t, f.Truncate(a.Size))
					require.NoError(t, f.Close())
				}
				path := filepath.Join(model, "model.onnx")
				switch tc.invalid {
				case "short":
					require.NoError(t, os.Truncate(path, 7))
				case "empty":
					require.NoError(t, os.Truncate(path, 0))
				case "directory":
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.Mkdir(path, 0o700))
				}
			}
			if tc.name != "missing directory" {
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".install-stale", "bin"), 0o700))
			}
			start := time.Now()
			got, err := Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
			require.NoError(t, err)
			assert.Equal(t, Status{DownloadBytes: tc.bytes}, got)
			assert.Less(t, time.Since(start), time.Second)
			if tc.name != "missing directory" {
				assert.DirExists(t, filepath.Join(dir, ".install-stale"))
			}
		})
	}
}

func TestInspectOllama(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		code                  int
		skip, stall, redirect bool
		available             bool
	}{
		{name: "supported", code: 200, available: true},
		{name: "missing model", code: 404},
		{name: "failure", code: 503},
		{name: "skipped", code: 200, skip: true},
		{name: "stalled", stall: true},
		{name: "redirect", redirect: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var probes, redirects atomic.Int32
			release := make(chan struct{})
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirects.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				probes.Add(1)
				assert.Equal(t, "/api/show", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				if tc.stall {
					<-release
					return
				}
				if tc.redirect {
					http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
					return
				}
				w.WriteHeader(tc.code)
			}))
			defer func() { close(release); server.Close() }()
			start := time.Now()
			got, err := inspect(t.Context(), Options{DataDir: t.TempDir(), SkipOllama: tc.skip}, server.URL)
			require.NoError(t, err)
			assert.Equal(t, tc.available, got.OllamaAvailable)
			assert.Equal(t, int64(294570424), got.DownloadBytes)
			assert.Less(t, time.Since(start), time.Second)
			assert.Zero(t, redirects.Load())
			want := int32(1)
			if tc.skip {
				want = 0
			}
			assert.Equal(t, want, probes.Load())
		})
	}
}

func TestInspectCancellationAndErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Inspect(ctx, Options{DataDir: t.TempDir(), SkipOllama: true})
	require.ErrorIs(t, err, context.Canceled)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-release }))
	defer func() { close(release); server.Close() }()
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	_, err = inspect(ctx, Options{DataDir: t.TempDir()}, server.URL)
	require.ErrorIs(t, err, context.Canceled)
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "bin"), []byte("file"), 0o600))
	_, err = Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
	require.Error(t, err)
	_, err = Inspect(t.Context(), Options{DataDir: filepath.Join(dir, "bin", "nested"), SkipOllama: true})
	require.Error(t, err)
}

func TestInspectInstallInAnotherProcess(t *testing.T) {
	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, "-test.run=^TestInspectLockProcess$", "--", dir)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	stdin, err := cmd.StdinPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	defer func() { _ = stdin.Close(); require.NoError(t, cmd.Wait()) }()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)
	start := time.Now()
	got, err := Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
	require.NoError(t, err)
	assert.Equal(t, Status{Installing: true, DownloadBytes: 294570424}, got)
	assert.Less(t, time.Since(start), time.Second)
}

func TestInspectLockProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	release, err := (daemon.RuntimeStore{Dir: os.Args[len(os.Args)-1], Prefix: "kit-embed-install"}).AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	_, err = os.Stdout.WriteString("locked\n")
	require.NoError(t, err)
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}
