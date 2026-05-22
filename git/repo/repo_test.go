package gitrepo

import (
	"context"
	"os"
	"testing"

	gittest "go.kenn.io/kit/git/test"
)

func TestIsUnbornHead(t *testing.T) {
	repo := gittest.NewRepo(t, gittest.Options{ConfigureUser: true})
	if !IsUnbornHead(context.Background(), repo.Root) {
		t.Fatal("newly initialized repo should have unborn HEAD")
	}

	repo.CommitFile("a.txt", "a\n", "initial")
	if IsUnbornHead(context.Background(), repo.Root) {
		t.Fatal("repo with a commit should not have unborn HEAD")
	}
}

func TestIsAncestorDistinguishesNegativeFromGitErrors(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	base := repo.Head()
	repo.CommitFile("b.txt", "b\n", "second")
	head := repo.Head()

	ok, err := IsAncestor(context.Background(), repo.Root, base, head)
	if err != nil || !ok {
		t.Fatalf("base should be ancestor of head: ok=%v err=%v", ok, err)
	}

	ok, err = IsAncestor(context.Background(), repo.Root, head, base)
	if err != nil {
		t.Fatalf("non-ancestor should not be an error: %v", err)
	}
	if ok {
		t.Fatal("head should not be ancestor of base")
	}
}

func TestWorktreePathForBranchSkipsStalePaths(t *testing.T) {
	repo := gittest.NewRepoWithCommit(t)
	wt := repo.AddWorktree("feature")

	path, ok, err := WorktreePathForBranch(context.Background(), repo.Root, "feature")
	if err != nil || !ok || path != wt {
		t.Fatalf("path=%q ok=%v err=%v, want %q true nil", path, ok, err, wt)
	}

	if err := os.RemoveAll(wt); err != nil {
		t.Fatal(err)
	}
	path, ok, err = WorktreePathForBranch(context.Background(), repo.Root, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if ok || path != repo.Root {
		t.Fatalf("stale worktree should fall back to repo root false, got %q %v", path, ok)
	}
}

func TestLooksLikeSHAUsesLengthAndHexPattern(t *testing.T) {
	for _, value := range []string{"abcdef0", "ABCDEF0123456789"} {
		if !LooksLikeSHA(value) {
			t.Fatalf("%q should look like a SHA", value)
		}
	}
	for _, value := range []string{"dead", "not-a-sha", "abcdefg"} {
		if LooksLikeSHA(value) {
			t.Fatalf("%q should not look like a SHA", value)
		}
	}
}
