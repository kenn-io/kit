package gitcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSafeDirectoryFingerprintRejectsFIFO(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo unavailable")
	}
	path := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, exec.CommandContext(t.Context(), mkfifo, path).Run())
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeNamedPipe == 0 {
		t.Skip("mkfifo does not create native pipes")
	}
	done := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err == nil {
			err = writer.Close()
		}
		done <- err
	}()
	t.Cleanup(func() {
		reader, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		require.NoError(t, err)
		require.NoError(t, <-done)
		require.NoError(t, reader.Close())
	})
	_, err = safeDirectoryFingerprint([]string{path}, "")
	require.ErrorContains(t, err, "not a regular file")
}

func TestCachedTrust(t *testing.T) {
	originalTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 30 * time.Second
	t.Cleanup(func() { safeDirectoryProbeTimeout = originalTimeout })
	for _, noSystem := range []string{"0", "1"} {
		t.Run("no system "+noSystem, func(t *testing.T) {
			dir := t.TempDir()
			config, otherConfig := filepath.Join(dir, "gitconfig"), filepath.Join(dir, "other")
			trace := filepath.Join(dir, "trace")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
			require.NoError(t, os.WriteFile(otherConfig, []byte("[safe]\n directory = /other\n"), 0o600))
			runner := New()
			runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_TRACE="+filepath.ToSlash(trace), "GIT_CONFIG_NOSYSTEM="+noSystem)
			other := runner
			other.Env = append(append([]string(nil), runner.Env...), "GIT_CONFIG_GLOBAL="+otherConfig)
			count := 0
			check := func(r Runner, want string, increment int) {
				assert.Equal(t, []string{want}, r.trust.read(t.Context(), r.Env, dir))
				contents, err := os.ReadFile(trace)
				require.NoError(t, err)
				actual := strings.Count(string(contents), "built-in:")
				assert.Equal(t, count+increment, actual)
				count = actual
			}
			baseline := 2
			if noSystem == "1" {
				baseline = 1
			}
			check(runner, "/trusted", baseline)
			check(other, "/other", baseline)
			check(runner, "/trusted", 2*baseline)
			check(other, "/other", baseline)
			check(runner, "/trusted", 0)
			check(runner.WithConfig("gc.auto", "1"), "/trusted", 0)
			fresh := New()
			fresh.Env = runner.Env
			check(fresh, "/trusted", baseline)
		})
	}
	t.Run("invalidation", func(t *testing.T) {
		dir := t.TempDir()
		config := filepath.Join(dir, "gitconfig")
		runner := New()
		runner.Env = safeDirectoryTestEnv(t, config)
		read := func() []string {
			return runner.trust.read(t.Context(), runner.Env, dir)
		}
		assert.Empty(t, read())
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /first\n"), 0o600))
		assert.Equal(t, []string{"/first"}, read())
		info, err := os.Stat(config)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
		require.NoError(t, os.Chtimes(config, info.ModTime(), info.ModTime()))
		assert.Equal(t, []string{"/other"}, read())
		require.NoError(t, os.Remove(config))
		assert.Empty(t, read())
	})
	t.Run("relative paths", func(t *testing.T) {
		runner := New()
		runner.Env = append(safeDirectoryTestEnv(t, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		for _, trust := range []string{"/one", "/two", "/one"} {
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "gitconfig"), []byte("[safe]\n directory = "+trust+"\n"), 0o600))
			assert.Equal(t, []string{trust}, runner.trust.read(t.Context(), runner.Env, dir))
		}
	})
	t.Run("includes", func(t *testing.T) {
		dir := t.TempDir()
		config := filepath.Join(dir, "gitconfig")
		included := filepath.Join(dir, "included")
		require.NoError(t, os.WriteFile(config, []byte("[include]\n path = included\n"), 0o600))
		runner := New()
		runner.Env = safeDirectoryTestEnv(t, config)
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
		for _, trust := range []string{"/first", "/other", ""} {
			require.NoError(t, os.WriteFile(included, []byte("[safe]\n directory = "+trust+"\n"), 0o600))
			assert.Equal(t, []string{trust}, runner.trust.read(t.Context(), runner.Env, dir))
		}
		require.NoError(t, os.WriteFile(config, []byte("[includeIf \"onbranch:trusted\"]\n path = included\n"), 0o600))
		require.NoError(t, os.WriteFile(included, []byte("[safe]\n directory = /branch\n"), 0o600))
		_, err := runner.Output(t.Context(), dir, "init", "-b", "trusted")
		require.NoError(t, err)
		assert.Equal(t, []string{"/branch"}, runner.trust.read(t.Context(), runner.Env, dir))
		_, err = runner.Output(t.Context(), dir, "symbolic-ref", "HEAD", "refs/heads/other")
		require.NoError(t, err)
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
	})
}

func coordinatedTrustGit(t *testing.T) (Runner, chan chan struct{}) {
	t.Helper()
	originalTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 30 * time.Second
	t.Cleanup(func() { safeDirectoryProbeTimeout = originalTimeout })
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	started := make(chan chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		release := make(chan struct{})
		started <- release
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	bin := buildTestGit(t, `package main
import ("net/http"; "os"; "os/exec")
func main() {
 if len(os.Args) > 1 && os.Args[1] == "config" {
  response, err := http.Get(os.Getenv("TRUST_TEST_URL"))
  if err != nil { os.Exit(2) }; response.Body.Close()
 }
 cmd := exec.Command(os.Getenv("TRUST_TEST_GIT"), os.Args[1:]...)
 cmd.Stdout, cmd.Stderr, cmd.Env = os.Stdout, os.Stderr, os.Environ()
 if err := cmd.Run(); err != nil { os.Exit(1) }
}
`)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
	runner := New()
	runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_CONFIG_NOSYSTEM=1", "TRUST_TEST_URL="+server.URL, "TRUST_TEST_GIT="+realGit)
	return runner, started
}

func TestCachedTrustConcurrentEvaluation(t *testing.T) {
	for _, mode := range []string{"canceled fill", "competing environments"} {
		t.Run(mode, func(t *testing.T) {
			runner, started := coordinatedTrustGit(t)
			if mode == "canceled fill" {
				result := make(chan []string, 1)
				go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				assert.Equal(t, []string{"/trusted"}, <-result)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go func() { result <- runner.trust.read(ctx, runner.Env, "") }()
				<-started
				cancel()
				assert.Empty(t, <-result)
				go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				assert.Equal(t, []string{"/trusted"}, <-result)
				return
			}
			other := runner
			config := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
			other.Env = append(append([]string(nil), runner.Env...), "GIT_CONFIG_GLOBAL="+config)
			one, two := make(chan []string, 1), make(chan []string, 1)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			close(<-started)
			assert.Equal(t, []string{"/trusted"}, <-one)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			first := <-started
			go func() { two <- other.trust.read(t.Context(), other.Env, "") }()
			second := <-started
			close(second)
			assert.Equal(t, []string{"/other"}, <-two)
			close(first)
			assert.Equal(t, []string{"/trusted"}, <-one)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			assert.Equal(t, []string{"/trusted"}, <-one)
		})
	}
}
