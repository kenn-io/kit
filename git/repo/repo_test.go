package gitrepo

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	gittest "go.kenn.io/kit/git/test"
)

func TestIsUnbornHead(t *testing.T) {
	repo := gittest.NewRepo(t, gittest.Options{ConfigureUser: true})
	if !IsUnbornHead(t.Context(), repo.Root) {
		require.FailNow(t, "newly initialized repo should have unborn HEAD")
	}

	repo.CommitFile("a.txt", "a\n", "initial")
	if IsUnbornHead(t.Context(), repo.Root) {
		require.FailNow(t, "repo with a commit should not have unborn HEAD")
	}
}

func TestIsAncestorDistinguishesNegativeFromGitErrors(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	base := repo.Head()
	repo.CommitFile("b.txt", "b\n", "second")
	head := repo.Head()

	ok, err := IsAncestor(t.Context(), repo.Root, base, head)
	if err != nil || !ok {
		require.FailNow(t, fmt.Sprintf("base should be ancestor of head: ok=%v err=%v", ok, err))
	}

	ok, err = IsAncestor(t.Context(), repo.Root, head, base)
	if err != nil {
		require.FailNow(t, fmt.Sprintf("non-ancestor should not be an error: %v", err))
	}
	if ok {
		require.FailNow(t, "head should not be ancestor of base")
	}
}

func TestWorktreePathForBranchSkipsStalePaths(t *testing.T) {
	require := require.New(t)
	repo := gittest.NewRepoWithCommit(t)
	wt := repo.AddWorktree("feature")

	path, ok, err := WorktreePathForBranch(t.Context(), repo.Root, "feature")
	if err != nil || !ok || path != wt {
		require.FailNow(fmt.Sprintf("path=%q ok=%v err=%v, want %q true nil", path, ok, err, wt))
	}

	if err := os.RemoveAll(wt); err != nil {
		require.FailNow(err.Error())
	}
	path, ok, err = WorktreePathForBranch(t.Context(), repo.Root, "feature")
	if err != nil {
		require.FailNow(err.Error())
	}
	if ok || path != repo.Root {
		require.FailNow(fmt.Sprintf("stale worktree should fall back to repo root false, got %q %v", path, ok))
	}
}

func TestMainRootReturnsCheckoutRootForBareBackedWorktree(t *testing.T) {
	ctx := t.Context()
	bareRepo := gittest.NewBareRepo(t)
	seedRepo := gittest.NewRepoWithCommit(t)
	seedRepo.Run("remote", "add", "origin", bareRepo.Root)
	seedRepo.Run("push", "origin", "HEAD:main")

	worktreeDir := filepath.Join(t.TempDir(), "worktree")
	bareRepo.Run("worktree", "add", worktreeDir, "main")
	t.Cleanup(func() {
		_, _, _ = bareRepo.Runner.Run(ctx, bareRepo.Root, nil, "worktree", "remove", "--force", worktreeDir)
	})

	got, err := MainRoot(ctx, worktreeDir)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	want, err := Root(ctx, worktreeDir)
	if err != nil {
		require.FailNow(t, err.Error())
	}
	if got != want {
		require.FailNow(t, fmt.Sprintf("MainRoot() = %q, want checkout root %q", got, want))
	}
}

func TestLooksLikeSHAUsesLengthAndHexPattern(t *testing.T) {
	for _, value := range []string{"abcdef0", "ABCDEF0123456789"} {
		if !LooksLikeSHA(value) {
			require.FailNow(t, fmt.Sprintf("%q should look like a SHA", value))
		}
	}
	for _, value := range []string{"dead", "not-a-sha", "abcdefg"} {
		if LooksLikeSHA(value) {
			require.FailNow(t, fmt.Sprintf("%q should not look like a SHA", value))
		}
	}
}
