package gitcmd

import (
	"context"
	"os"
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

func TestCachedTrustCanceledWaitRetries(t *testing.T) {
	runner := New()
	config := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
	runner.Env = safeDirectoryTestEnv(t, config)
	runner.trust.gate <- struct{}{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	assert.Empty(t, runner.trust.read(ctx, runner.Env, ""))
	<-runner.trust.gate
	assert.Empty(t, runner.trust.read(ctx, runner.Env, ""))
	assert.Equal(t, []string{"/trusted"}, runner.trust.read(t.Context(), runner.Env, ""))
}

func TestCachedTrustCanceledFillRetries(t *testing.T) {
	path := os.Getenv("PATH")
	t.Setenv("PATH", buildSleepingGit(t)+string(os.PathListSeparator)+path)
	runner := New()
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	assert.Empty(t, runner.trust.read(ctx, runner.Env, ""))
	t.Setenv("PATH", path)
	config := filepath.Join(t.TempDir(), "gitconfig")
	require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
	runner.Env = safeDirectoryTestEnv(t, config)
	assert.Equal(t, []string{"/trusted"}, runner.trust.read(t.Context(), runner.Env, ""))
}
