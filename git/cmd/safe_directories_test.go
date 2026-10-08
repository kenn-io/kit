package gitcmd

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCachedTrustFilesystemBound(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				original := &safeDirectorySnapshot{}
				cache := &safeDirectoryCache{current: original}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started, release := make(chan struct{}), make(chan struct{})
				result := make(chan bool, 1)
				var late *safeDirectorySnapshot
				go func() {
					result <- cache.evaluateFilesystem(ctx, func() {
						close(started)
						<-release
						late = &safeDirectorySnapshot{discoveryFailed: true}
					})
				}()
				<-started
				if mode == "cancel" {
					cancel()
				} else {
					time.Sleep(safeDirectoryProbeTimeout)
				}
				synctest.Wait()
				require.False(t, <-result)
				called := false
				assert.False(t, cache.evaluateFilesystem(t.Context(), func() { called = true }))
				assert.False(t, called)
				assert.Same(t, original, cache.current)
				close(release)
				synctest.Wait()
				assert.NotNil(t, late)
				assert.Same(t, original, cache.current)
				assert.True(t, cache.evaluateFilesystem(t.Context(), func() { called = true }))
				assert.True(t, called)
			})
		})
	}
}

func extendTrustProbeTimeout(t *testing.T) {
	t.Helper()
	originalTimeout := safeDirectoryProbeTimeout
	safeDirectoryProbeTimeout = 30 * time.Second
	t.Cleanup(func() { safeDirectoryProbeTimeout = originalTimeout })
}

func TestCachedTrust(t *testing.T) {
	extendTrustProbeTimeout(t)
	for _, tc := range []struct {
		name, noSystem string
		discoveryFails bool
	}{{"no system 0", "0", false}, {"no system 1", "1", false}, {"failed discovery", "0", true}} {
		t.Run(tc.name, func(t *testing.T) {
			noSystem := tc.noSystem
			dir := t.TempDir()
			config, otherConfig := filepath.Join(dir, "config..old"), filepath.Join(dir, "other")
			trace := filepath.Join(dir, "trace")
			require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /trusted\n"), 0o600))
			require.NoError(t, os.WriteFile(otherConfig, []byte("[safe]\n directory = /other\n"), 0o600))
			runner := New()
			runner.Env = append(safeDirectoryTestEnv(t, config), "GIT_TRACE="+filepath.ToSlash(trace), "GIT_CONFIG_NOSYSTEM="+noSystem, "HOME="+dir+"/unused/..\n", "XDG_CONFIG_HOME="+dir+"/unused/..\r")
			system, _ := envValue(runner.Env, "GIT_CONFIG_SYSTEM")
			if noSystem == "0" {
				system = filepath.Dir(system) + "/unused/../" + filepath.Base(system)
			} else {
				system = "/inactive\nconfig\r"
			}
			runner.Env = append(runner.Env, "GIT_CONFIG_SYSTEM="+system)
			if tc.discoveryFails {
				runner.Env = append(runner.Env, "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=core.autocrlf", "GIT_CONFIG_VALUE_0=garbage")
			}
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
			if tc.discoveryFails {
				check(runner, "/trusted", baseline+1)
				check(runner, "/trusted", baseline)
				require.NoError(t, os.WriteFile(config, []byte("[safe]\n directory = /updated\n"), 0o600))
				check(runner, "/updated", baseline)
				return
			}
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
	t.Run("home share", func(t *testing.T) {
		if runtime.GOOS != "windows" {
			t.Skip("requires Git for Windows HOME selection")
		}
		dir := t.TempDir()
		share, profile := filepath.Join(dir, "share"), filepath.Join(dir, "profile")
		for _, home := range []string{share, profile} {
			require.NoError(t, os.Mkdir(home, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[safe]\n directory = /"+filepath.Base(home)+"\n"), 0o600))
		}
		runner := New()
		runner.Env = slices.DeleteFunc(safeDirectoryTestEnv(t, ""), func(entry string) bool {
			key, _, _ := strings.Cut(entry, "=")
			return strings.EqualFold(key, "HOME") || strings.EqualFold(key, "GIT_CONFIG_GLOBAL") || strings.EqualFold(key, "XDG_CONFIG_HOME")
		})
		drive := filepath.VolumeName(share)
		runner.Env = append(runner.Env, "GIT_CONFIG_NOSYSTEM=1", "HOMEDRIVE="+drive, "HOMEPATH="+strings.TrimPrefix(share, drive), "USERPROFILE="+profile)
		read := func(want string) {
			assert.Equal(t, []string{want}, runner.trust.read(t.Context(), runner.Env, dir))
		}
		for range 3 {
			read("/share")
		}
		require.NoError(t, os.Rename(share, share+"-offline"))
		read("/profile")
		require.NoError(t, os.WriteFile(filepath.Join(profile, ".gitconfig"), []byte("[safe]\n directory = /updated\n"), 0o600))
		read("/updated")
		require.NoError(t, os.Rename(share+"-offline", share))
		read("/share")
	})
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
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub")
		require.NoError(t, os.Mkdir(sub, 0o700))
		for _, path := range []string{filepath.Join(dir, "gitconfig"), filepath.Join(sub, "gitconfig")} {
			require.NoError(t, os.WriteFile(path, []byte("[safe]\n directory = /trusted\n"), 0o600))
		}
		runner := New()
		runner.Env = append(safeDirectoryTestEnv(t, "gitconfig"), "GIT_CONFIG_NOSYSTEM=1")
		runner.DisableSafeDirectoryForward = true
		_, err := runner.Output(t.Context(), dir, "init")
		require.NoError(t, err)
		for range 3 {
			assert.Equal(t, []string{"/trusted"}, runner.trust.read(t.Context(), runner.Env, sub))
		}
		require.NoError(t, os.WriteFile(filepath.Join(dir, "gitconfig"), nil, 0o600))
		fresh := readSafeDirectories(t.Context(), runner.Env, sub)
		assert.Empty(t, fresh)
		assert.Equal(t, fresh, runner.trust.read(t.Context(), runner.Env, sub))
	})
	t.Run("includes", func(t *testing.T) {
		dir := t.TempDir()
		config := filepath.Join(dir, "gitconfig")
		included := filepath.Join(dir, "included")
		require.NoError(t, os.WriteFile(config, []byte("[include]\n path = included\n"), 0o600))
		runner := New()
		runner.Env = safeDirectoryTestEnv(t, config)
		assert.Empty(t, runner.trust.read(t.Context(), runner.Env, dir))
		for _, trust := range []string{"/first", "/other"} {
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
	t.Run("ambiguous paths", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("requires native Unix path resolution")
		}
		for _, tc := range []struct{ key, suffix, component string }{
			{"GIT_CONFIG_GLOBAL", "config", "link/.."},
			{"GIT_CONFIG_GLOBAL", "config", "line\npart"},
			{"HOME", ".gitconfig", "line\npart"},
			{"XDG_CONFIG_HOME", "git/config", "line\npart"},
			{"GIT_CONFIG_SYSTEM", "config", "line\npart"},
			{"GIT_CONFIG_GLOBAL", "config", "line\rpart"},
			{"GIT_CONFIG_SYSTEM", "config", "line\rpart"},
		} {
			t.Run(tc.key+" "+strings.ReplaceAll(strings.ReplaceAll(tc.component, "\n", "LF"), "\r", "CR"), func(t *testing.T) {
				dir := t.TempDir()
				target := filepath.Join(dir, tc.component)
				if tc.component == "link/.." {
					target = filepath.Join(dir, "target")
					require.NoError(t, os.MkdirAll(filepath.Join(target, "nested"), 0o700))
					require.NoError(t, os.Symlink(filepath.Join(target, "nested"), filepath.Join(dir, "link")))
				}
				actual, decoy := filepath.Join(target, tc.suffix), filepath.Join(dir, tc.suffix)
				for _, path := range []string{actual, decoy} {
					require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
					require.NoError(t, os.WriteFile(path, []byte("[safe]\n directory = /trusted\n"), 0o600))
				}
				raw := dir + "/" + tc.component
				if tc.key == "GIT_CONFIG_GLOBAL" || tc.key == "GIT_CONFIG_SYSTEM" {
					raw += "/" + tc.suffix
				}
				env := slices.DeleteFunc(safeDirectoryTestEnv(t, actual), func(entry string) bool { return strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL=") })
				env = append(env, "HOME="+dir, "XDG_CONFIG_HOME="+filepath.Join(dir, "xdg"), tc.key+"="+raw)
				if tc.key == "GIT_CONFIG_SYSTEM" {
					env = append(env, "GIT_CONFIG_GLOBAL="+filepath.Join(dir, "absent"))
				}
				runner := New()
				runner.Env = env
				for range 3 {
					assert.Equal(t, []string{"/trusted"}, runner.trust.read(t.Context(), env, dir))
				}
				require.NoError(t, os.WriteFile(actual, nil, 0o600))
				fresh := readSafeDirectories(t.Context(), env, dir)
				assert.Empty(t, fresh)
				assert.Equal(t, fresh, runner.trust.read(t.Context(), env, dir))
				require.NoError(t, os.WriteFile(actual, []byte("[safe]\n directory = /updated\n"), 0o600))
				assert.Equal(t, []string{"/updated"}, runner.trust.read(t.Context(), env, dir))
			})
		}
	})
}

func coordinatedTrustGit(t *testing.T) (Runner, chan chan struct{}) {
	t.Helper()
	extendTrustProbeTimeout(t)
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
 if len(os.Args) > 1 && (os.Args[1] == "config" || os.Args[1] == "var" && os.Getenv("TRUST_TEST_BLOCK_VAR") == "1") {
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
	for _, mode := range []string{"canceled fill", "canceled discovery", "competing environments"} {
		t.Run(mode, func(t *testing.T) {
			runner, started := coordinatedTrustGit(t)
			if mode != "competing environments" {
				if mode == "canceled discovery" {
					runner.Env = append(runner.Env, "TRUST_TEST_BLOCK_VAR=1")
				}
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
				assert.False(t, runner.trust.current.discoveryFailed)
				go func() { result <- runner.trust.read(t.Context(), runner.Env, "") }()
				close(<-started)
				if mode == "canceled discovery" {
					close(<-started)
				}
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
