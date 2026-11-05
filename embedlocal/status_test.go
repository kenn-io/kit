package embedlocal

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/daemon"
	"go.kenn.io/kit/internal/embedassets"
)

func TestInspectFiles(t *testing.T) {
	var totalModelBytes, onnxBytes int64
	for _, asset := range embedassets.ModelAssets() {
		totalModelBytes += asset.Size
		if asset.Name == "model.onnx" {
			onnxBytes = asset.Size
		}
	}
	for _, tc := range []struct {
		name          string
		bundle, model bool
		invalid       string
		bytes         int64
	}{
		{name: "missing directory", bytes: totalModelBytes + bundleDownloadBytes},
		{name: "complete", bundle: true, model: true},
		{name: "model missing", bundle: true, bytes: totalModelBytes},
		{name: "bundle missing", model: true, bytes: bundleDownloadBytes},
		{name: "short model", bundle: true, model: true, invalid: "short", bytes: onnxBytes},
		{name: "model directory", bundle: true, model: true, invalid: "directory", bytes: onnxBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "shared")
			if tc.bundle {
				bundle := filepath.Join(dir, "bin", embedassets.BundleTag)
				require.NoError(t, os.MkdirAll(bundle, 0o700))
				for _, name := range append(embedassets.NativeFiles(), embedassets.ExecutableName()) {
					require.NoError(t, os.WriteFile(filepath.Join(bundle, name), []byte("fixture"), 0o700))
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
				case "directory":
					require.NoError(t, os.Remove(path))
					require.NoError(t, os.Mkdir(path, 0o700))
				}
			}
			got, err := Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
			require.NoError(t, err)
			assert.Equal(t, Status{DownloadBytes: tc.bytes}, got)
			if tc.name == "missing directory" {
				assert.NoDirExists(t, dir)
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
	_, err = Inspect(t.Context(), Options{DataDir: t.TempDir() + "\x00", SkipOllama: true})
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
	waited := false
	defer func() {
		_ = stdin.Close()
		if !waited {
			_ = cmd.Wait()
		}
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "locked\n", line)
	start := time.Now()
	got, err := Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
	require.NoError(t, err)
	wantBytes := int64(bundleDownloadBytes)
	for _, asset := range embedassets.ModelAssets() {
		wantBytes += asset.Size
	}
	assert.Equal(t, Status{Installing: true, DownloadBytes: wantBytes}, got)
	assert.Less(t, time.Since(start), time.Second)
	require.NoError(t, stdin.Close())
	require.NoError(t, cmd.Wait())
	waited = true
	got, err = Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
	require.NoError(t, err)
	assert.False(t, got.Installing, "leftover staging is idle after the installer exits")
}

func TestInspectLockProcess(t *testing.T) {
	if len(os.Args) < 3 || os.Args[len(os.Args)-2] != "--" {
		return
	}
	release, err := (daemon.RuntimeStore{Dir: os.Args[len(os.Args)-1], Prefix: "kit-embed-install"}).AcquireStartLock(t.Context())
	require.NoError(t, err)
	defer release()
	require.NoError(t, os.Mkdir(filepath.Join(os.Args[len(os.Args)-1], ".install-active"), 0o700))
	_, err = os.Stdout.WriteString("locked\n")
	require.NoError(t, err)
	var b [1]byte
	_, _ = os.Stdin.Read(b[:])
}

func TestInspectInstallApproval(t *testing.T) {
	client, assets, _, _ := installFixture(t, "", nil)
	dir := t.TempDir()
	var confirmed, progressed bool
	_, err := install(t.Context(), InstallOptions{
		DataDir: dir,
		Confirm: func(ctx context.Context, _ int64) (bool, error) {
			got, err := Inspect(ctx, Options{DataDir: dir, SkipOllama: true})
			require.NoError(t, err)
			assert.False(t, got.Installing, "confirmation precedes approved installation")
			confirmed = true
			return true, nil
		},
		Progress: func(downloaded, _ int64) {
			if downloaded != 0 {
				return
			}
			got, err := Inspect(t.Context(), Options{DataDir: dir, SkipOllama: true})
			require.NoError(t, err)
			assert.True(t, got.Installing, "approved work owns the lock and staging directory")
			progressed = true
		},
	}, client, assets)
	require.NoError(t, err)
	assert.True(t, confirmed)
	assert.True(t, progressed)
}
