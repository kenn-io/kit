package gitcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedTrustInvalidation(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "gitconfig")
	runner := New()
	runner.Env = safeDirectoryTestEnv(t, config)
	read := func() string {
		return gitConfigValue(strings.Join(runner.Command(t.Context(), dir, "status").Env, "\n"), "safe.directory")
	}
	assert.Empty(t, read())
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /first\n"), 0o600))
	assert.Equal(t, "/first", read())
	info, err := os.Stat(config)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
	require.NoError(t, os.Chtimes(config, info.ModTime(), info.ModTime()))
	assert.Equal(t, "/other", read())
	require.NoError(t, os.Remove(config))
	assert.Empty(t, read())
	other := filepath.Join(dir, "other")
	require.NoError(t, os.WriteFile(other, []byte("[safe]\n directory = /second\n"), 0o600))
	runner.Env = append(runner.Env, "GIT_CONFIG_GLOBAL="+other)
	assert.Equal(t, "/second", read())
	for _, entry := range runner.Env {
		key, value, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			t.Setenv(key, value)
		}
	}
	runner.Env = nil
	assert.Equal(t, "/second", read())
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	assert.Empty(t, read())
	if os.Symlink(other, config) == nil {
		assert.Equal(t, "/second", read())
		require.NoError(t, os.Remove(config))
		empty := filepath.Join(dir, "empty")
		require.NoError(t, os.WriteFile(empty, nil, 0o600))
		require.NoError(t, os.Symlink(empty, config))
		assert.Empty(t, read())
	}
}

func TestCachedTrustRelativePaths(t *testing.T) {
	runner := New()
	runner.Env = append(safeDirectoryTestEnv(t, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
	for _, trust := range []string{"/one", "/two", "/one"} {
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gitconfig"), []byte("[safe]\n directory = "+trust+"\n"), 0o600))
		assert.Equal(t, trust, gitConfigValue(strings.Join(runner.Command(t.Context(), dir, "status").Env, "\n"), "safe.directory"))
	}
}

func TestCachedTrustKeepsIncludesFresh(t *testing.T) {
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
}

func TestCachedTrustCanceledFillRetries(t *testing.T) {
	runner, started := coordinatedTrustGit(t)
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
	for _, includes := range []bool{false, true} {
		t.Run(map[bool]string{false: "competing environments", true: "includes"}[includes], func(t *testing.T) {
			runner, started := coordinatedTrustGit(t)
			other := runner
			config := filepath.Join(t.TempDir(), "gitconfig")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /other\n"), 0o600))
			other.Env = append(append([]string(nil), runner.Env...), "GIT_CONFIG_GLOBAL="+config)
			if includes {
				require.NoError(t, os.WriteFile(config, []byte("[include]\n path = missing\n[safe]\n directory = /other\n"), 0o600))
				runner = other
			}
			one, two := make(chan []string, 1), make(chan []string, 1)
			want := "/trusted"
			warmCalls := 1
			if includes {
				warmCalls = 2
				want = "/other"
			}
			for range warmCalls {
				go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				assert.Equal(t, []string{want}, <-one)
			}
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			first := <-started
			go func() { two <- other.trust.read(t.Context(), other.Env, "") }()
			second := <-started
			close(second)
			assert.Equal(t, []string{"/other"}, <-two)
			close(first)
			assert.Equal(t, []string{want}, <-one)
			go func() { one <- runner.trust.read(t.Context(), runner.Env, "") }()
			if includes {
				close(<-started)
			}
			assert.Equal(t, []string{want}, <-one)
		})
	}
}
