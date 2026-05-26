// Package gittest creates isolated git repositories for tests.
//
// Repositories created here use the shared defensive git runner, local test
// identity, and cleanup hooks for linked worktrees. This keeps tests from
// inheriting a parent hook or GIT_* environment and accidentally mutating the
// checkout that is running the test.
package gittest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitcmd "go.kenn.io/kit/git/cmd"
)

const (
	// UserName is the default git author and committer name for test repos.
	UserName = "Test User"
	// UserEmail is the default git author and committer email for test repos.
	UserEmail = "test@example.invalid"
)

// Repo is a temporary git repository owned by a test.
type Repo struct {
	// T is the test that owns the repository.
	T testing.TB
	// Root is the repository working tree or bare repository directory.
	Root string
	// GitDir is the expected .git directory for non-bare repositories.
	GitDir string
	// Runner executes git commands with isolated environment defaults.
	Runner gitcmd.Runner
}

// Options configures NewRepo.
type Options struct {
	// Dir is the directory to initialize. When empty, t.TempDir is used.
	Dir string
	// InitArgs are passed to git. When empty, []string{"init"} is used.
	InitArgs []string
	// ConfigureUser sets user.name and user.email to stable test values.
	ConfigureUser bool
	// ResolvePath stores EvalSymlinks(Dir) in Repo.Root when possible.
	ResolvePath bool
}

// NewRepo creates a git repository with sanitized git environment.
func NewRepo(t testing.TB, opts Options) *Repo {
	t.Helper()

	dir := opts.Dir
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create repo dir %q: %v", dir, err)
	}
	if opts.ResolvePath {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			dir = resolved
		}
	}
	repo := &Repo{
		T:      t,
		Root:   dir,
		GitDir: filepath.Join(dir, ".git"),
		Runner: gitcmd.New(),
	}
	if len(opts.InitArgs) == 0 {
		opts.InitArgs = []string{"init"}
	}
	repo.Run(opts.InitArgs...)
	if opts.ConfigureUser {
		repo.Config("user.email", UserEmail)
		repo.Config("user.name", UserName)
	}
	return repo
}

// NewRepoWithCommit creates a repository on main with one commit.
func NewRepoWithCommit(t testing.TB) *Repo {
	t.Helper()
	repo := NewRepo(t, Options{
		InitArgs:      []string{"init", "-b", "main"},
		ConfigureUser: true,
		ResolvePath:   true,
	})
	repo.CommitFile("base.txt", "base\n", "base commit")
	return repo
}

// NewBareRepo creates a bare repository.
func NewBareRepo(t testing.TB) *Repo {
	t.Helper()
	return NewRepo(t, Options{InitArgs: []string{"init", "--bare"}})
}

// Run runs git in the repo and returns trimmed stdout.
func (r *Repo) Run(args ...string) string {
	r.T.Helper()
	out, _, err := r.RunRaw(args...)
	if err != nil {
		r.T.Fatalf("git %v failed: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

// RunRaw runs git in the repo and returns stdout, stderr, and error.
func (r *Repo) RunRaw(args ...string) ([]byte, []byte, error) {
	r.T.Helper()
	return r.Runner.Run(context.Background(), r.Root, nil, args...)
}

// Config sets a local git config value.
func (r *Repo) Config(key, value string) {
	r.T.Helper()
	r.Run("config", key, value)
}

// WriteFile writes a repository-relative file.
func (r *Repo) WriteFile(name, content string) {
	r.T.Helper()
	path := filepath.Join(r.Root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.T.Fatalf("mkdir %q: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.T.Fatalf("write %q: %v", path, err)
	}
}

// CommitFile writes, stages, commits, and returns HEAD.
func (r *Repo) CommitFile(name, content, msg string) string {
	r.T.Helper()
	r.WriteFile(name, content)
	r.Run("add", name)
	r.Run("commit", "-m", msg)
	return r.Head()
}

// Head returns HEAD's SHA.
func (r *Repo) Head() string {
	r.T.Helper()
	return r.Run("rev-parse", "HEAD")
}

// Checkout runs git checkout.
func (r *Repo) Checkout(args ...string) {
	r.T.Helper()
	r.Run(append([]string{"checkout"}, args...)...)
}

// AddWorktree creates a linked worktree on branch and removes it at cleanup.
func (r *Repo) AddWorktree(branch string) string {
	r.T.Helper()
	dir := r.T.TempDir()
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	r.Run("worktree", "add", dir, "-b", branch)
	r.T.Cleanup(func() {
		_, _, _ = r.Runner.Run(context.Background(), r.Root, nil, "worktree", "remove", "--force", dir)
	})
	return dir
}
