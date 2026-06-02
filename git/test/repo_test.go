package gittest

import (
	"path/filepath"
	"testing"
)

func TestNewRepoIgnoresPollutedGitEnvironment(t *testing.T) {
	parent := NewRepoWithCommit(t)
	t.Setenv("GIT_DIR", parent.GitDir)
	t.Setenv("GIT_WORK_TREE", parent.Root)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "Polluted")

	repo := NewRepoWithCommit(t)

	if repo.Root == parent.Root {
		t.Fatal("expected a distinct test repo")
	}
	if got := repo.Run("config", "user.name"); got != UserName {
		t.Fatalf("user.name = %q, want %q", got, UserName)
	}
	if got := filepath.Clean(repo.Run("rev-parse", "--show-toplevel")); got != filepath.Clean(repo.Root) {
		t.Fatalf("show-toplevel = %q, want %q", got, repo.Root)
	}
}
