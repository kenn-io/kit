package gitworktree

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	gitcmd "go.kenn.io/kit/git/cmd"
	gitenv "go.kenn.io/kit/git/env"
	gittest "go.kenn.io/kit/git/test"
)

func TestCreateCaptureAndApplyPatch(t *testing.T) {
	require := require.New(t)
	ctx := t.Context()
	repo := gittest.NewRepoWithCommit(t)

	wt, err := Create(ctx, repo.Root, "HEAD", Options{ParentDir: t.TempDir()})
	if err != nil {
		require.FailNow(err.Error())
	}
	t.Cleanup(func() { _ = wt.Close(ctx) })

	if err := os.WriteFile(filepath.Join(wt.Dir, "base.txt"), []byte("changed\n"), 0o644); err != nil {
		require.FailNow(err.Error())
	}
	patch, err := wt.CapturePatch(ctx)
	if err != nil {
		require.FailNow(err.Error())
	}
	if patch == "" {
		require.FailNow("expected non-empty patch")
	}
	if err := CheckPatch(ctx, repo.Root, patch); err != nil {
		require.FailNow(fmt.Sprintf("patch should apply cleanly: %v", err))
	}
	if err := ApplyPatch(ctx, repo.Root, patch); err != nil {
		require.FailNow(err.Error())
	}
	got, err := os.ReadFile(filepath.Join(repo.Root, "base.txt"))
	if err != nil {
		require.FailNow(err.Error())
	}
	if string(got) != "changed\n" {
		require.FailNow(fmt.Sprintf("base.txt = %q, want changed", got))
	}
	err = CheckPatch(ctx, repo.Root, patch)
	require.Error(err, "patch should conflict after being applied")
	require.ErrorAs(err, new(*PatchConflictError))
}

func TestGitmodulesFileProtocolDetection(t *testing.T) {
	tests := []struct {
		line string
		want bool
	}{
		{`url = "../local.git"`, true},
		{`url = file:///tmp/local.git`, true},
		{`url = https://github.com/acme/widget.git`, false},
		{`path = deps/widget`, false},
	}
	for _, tt := range tests {
		url, ok := ParseGitmodulesURL(tt.line)
		got := ok && IsFileProtocolURL(url)
		if got != tt.want {
			require.FailNow(t, fmt.Sprintf("line %q got %v want %v", tt.line, got, tt.want))
		}
	}
}

func traceRunner(t *testing.T) (gitcmd.Runner, func() [][]string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.json")
	runner := gitcmd.New()
	runner.DisableSafeDirectoryForward = true
	runner.StripEnv = false
	runner.Env = append(gitenv.StripAll(os.Environ()), "GIT_TRACE2_EVENT="+path)
	t.Cleanup(func() {
		if t.Failed() {
			data, err := os.ReadFile(path)
			t.Logf("Git trace (read error: %v):\n%s", err, data)
		}
	})
	return runner, func() [][]string {
		t.Helper()
		file, err := os.Open(path)
		require.NoError(t, err)
		defer file.Close()
		var commands [][]string
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			var event struct {
				Event string   `json:"event"`
				Argv  []string `json:"argv"`
			}
			require.NoError(t, json.Unmarshal(scanner.Bytes(), &event))
			if event.Event == "start" && len(event.Argv) > 0 {
				commands = append(commands, event.Argv[1:])
			}
		}
		require.NoError(t, scanner.Err())
		return commands
	}
}

func TestCreatePlainRepoGitStarts(t *testing.T) { //nolint:tparallel // Later cases compare their counts to the first case.
	t.Parallel()
	repo := gittest.NewRepoWithCommit(t)
	sha := repo.Head()
	baseline := 0
	for _, tc := range []struct {
		name  string
		setup bool
		base  string
	}{
		{name: "options off"},
		{name: "setup enabled", setup: true},
		{name: "resolved base", setup: true, base: sha},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner, starts := traceRunner(t)
			wt, err := Create(t.Context(), repo.Root, "HEAD", Options{ParentDir: t.TempDir(), InitSubmodules: tc.setup, PullLFS: tc.setup, BaseSHA: tc.base, Runner: runner})
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, wt.Close(context.Background())) //nolint:usetesting // Cleanup runs after the test context is canceled.
			})
			commands := starts()
			t.Logf("plain repo Git starts: %d", len(commands))
			require.Equal(t, sha, wt.BaseSHA)
			for _, args := range commands {
				require.NotContains(t, args, "submodule")
				require.NotContains(t, args, "lfs")
				if tc.base != "" {
					require.NotContains(t, args, "rev-parse")
				}
			}
			if !tc.setup {
				baseline = len(commands)
			} else {
				require.LessOrEqual(t, len(commands), baseline+2)
			}
		})
	}
}

func TestCreateSubmodules(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"real", "nested", "missing gitmodules", "defaults"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			repo := gittest.NewRepoWithCommit(t)
			dep := gittest.NewRepoWithCommit(t)
			if name == "nested" {
				nested := gittest.NewRepoWithCommit(t)
				dep.Run("-c", "protocol.file.allow=always", "submodule", "add", "file://"+filepath.ToSlash(nested.Root), "nested")
				dep.Run("commit", "-am", "add nested submodule")
			}
			if name == "missing gitmodules" {
				repo.Run("update-index", "--add", "--cacheinfo", "160000,"+dep.Head()+",dep")
			} else {
				repo.Run("-c", "protocol.file.allow=always", "submodule", "add", "file://"+filepath.ToSlash(dep.Root), "dep")
			}
			repo.Run("commit", "-m", "add submodule")
			runner, starts := traceRunner(t)
			parent := t.TempDir()
			wt, err := Create(t.Context(), repo.Root, "HEAD", Options{ParentDir: parent, InitSubmodules: name != "defaults", Runner: runner})
			if name == "missing gitmodules" {
				require.ErrorContains(t, err, "git submodule update")
				entries, readErr := os.ReadDir(parent)
				require.NoError(t, readErr)
				require.Empty(t, entries)
				return
			}
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, wt.Close(context.Background())) //nolint:usetesting // Cleanup runs after the test context is canceled.
			})
			commands := starts()
			if name == "defaults" {
				for _, args := range commands {
					require.NotContains(t, args, "submodule")
					require.NotContains(t, args, "lfs")
				}
				require.NoFileExists(t, filepath.Join(wt.Dir, "dep", "base.txt"))
				return
			}
			require.FileExists(t, filepath.Join(wt.Dir, "dep", "base.txt"))
			require.True(t, slices.ContainsFunc(commands, func(args []string) bool {
				return slices.Contains(args, "submodule") && slices.Contains(args, "update")
			}))
			if name == "nested" {
				entries, readErr := os.ReadDir(filepath.Join(wt.Dir, "dep", "nested"))
				require.NoError(t, readErr)
				require.Empty(t, entries)
			}
		})
	}
}

func TestCreateLFS(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"tracked file", "unmatched attributes", "missing git-lfs"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if name == "missing git-lfs" && runtime.GOOS == "windows" {
				t.Skip("Git for Windows adds bundled git-lfs to PATH")
			}
			repo := gittest.NewRepoWithCommit(t)
			repo.CommitFile(".gitattributes", "*.bin filter=lfs\n", "add LFS attributes")
			if name != "unmatched attributes" {
				repo.CommitFile("data.bin", "ordinary data\n", "add matching file")
			}
			runner, starts := traceRunner(t)
			if name == "missing git-lfs" {
				runner = runner.WithEnv(append(runner.Env, "PATH="+t.TempDir()))
			}
			_, _, versionErr := runner.Run(t.Context(), repo.Root, nil, "lfs", "version")
			if name == "missing git-lfs" {
				require.Error(t, versionErr)
			}
			before := len(starts())
			wt, err := Create(t.Context(), repo.Root, "HEAD", Options{ParentDir: t.TempDir(), PullLFS: true, Runner: runner})
			require.NoError(t, err)
			t.Cleanup(func() {
				require.NoError(t, wt.Close(context.Background())) //nolint:usetesting // Cleanup runs after the test context is canceled.
			})
			var env, pull bool
			for _, args := range starts()[before:] {
				for i := 0; i+1 < len(args); i++ {
					if args[i] == "lfs" {
						env = env || args[i+1] == "env"
						pull = pull || args[i+1] == "pull"
					}
				}
			}
			require.Equal(t, name != "unmatched attributes", env)
			require.Equal(t, name != "unmatched attributes" && versionErr == nil, pull)
		})
	}
}

func TestCreateResolvesSymbolicRefs(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepoWithCommit(t)
	for _, ref := range []string{"main", "HEAD"} {
		t.Run(ref, func(t *testing.T) {
			t.Parallel()
			runner, starts := traceRunner(t)
			wt, err := Create(t.Context(), repo.Root, ref, Options{ParentDir: t.TempDir(), Runner: runner})
			require.NoError(t, err)
			require.Equal(t, repo.Head(), wt.BaseSHA)
			require.True(t, slices.ContainsFunc(starts(), func(args []string) bool {
				return slices.Contains(args, "rev-parse")
			}))
			require.NoError(t, wt.Close(t.Context()))
		})
	}
}
