package managedworktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mergeDriverFixture struct {
	worktree string
	otherRef string
}

func newMergeDriverFixture(
	t *testing.T, base, current, other []byte,
	currentSubject, otherBranch string,
) mergeDriverFixture {
	t.Helper()
	require := require.New(t)

	if currentSubject == "" {
		currentSubject = "current change"
	}
	if otherBranch == "" {
		otherBranch = "merge-driver-other"
	}

	origin, clone := initOriginAndClone(t)
	require.NoError(os.WriteFile(
		filepath.Join(origin, ".gitattributes"),
		[]byte("payload merge=owned\n"), 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), base, 0o644,
	))
	require.NoError(os.WriteFile(
		filepath.Join(origin, "binary.dat"), []byte("\x00base\n"), 0o644,
	))
	lifecycleGit(t, origin, "add", ".gitattributes", "payload", "binary.dat")
	lifecycleGit(t, origin, "commit", "-qm", "merge driver base")

	const currentBranch = "merge-driver-current"
	lifecycleGit(t, origin, "checkout", "-q", "-b", currentBranch)
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), current, 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", currentSubject)
	currentSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")

	lifecycleGit(t, origin, "checkout", "-q", "main")
	lifecycleGit(t, origin, "checkout", "-q", "-b", otherBranch)
	require.NoError(os.WriteFile(
		filepath.Join(origin, "payload"), other, 0o644,
	))
	lifecycleGit(t, origin, "commit", "-qam", "other change")
	otherSHA := lifecycleGit(t, origin, "rev-parse", "HEAD")
	lifecycleGit(t, origin, "checkout", "-q", "main")

	otherRef := "refs/remotes/origin/" + otherBranch
	lifecycleGit(t, clone, "fetch", "-q", "origin",
		"refs/heads/"+otherBranch+":"+otherRef)
	require.Equal(otherSHA, lifecycleGit(t, clone, "rev-parse", otherRef))
	lifecycleGit(t, clone, "config", "merge.owned.driver", "false")

	worktree := filepath.Join(t.TempDir(), "worktree")
	_, err := CreateWorktreeFromMergeRequest(
		t.Context(), MergeRequestWorktreeOptions{
			Runner:              lifecycleTestRunner(t),
			ProjectRoot:         clone,
			Branch:              "imported-current",
			Path:                worktree,
			Number:              112,
			HeadBranch:          currentBranch,
			HeadRepoCloneURL:    origin,
			ProjectRepoIdentity: identityOfCloneURL(origin),
			ExpectedHeadSHA:     currentSHA,
		},
	)
	require.NoError(err)

	return mergeDriverFixture{worktree: worktree, otherRef: otherRef}
}

func TestUntrustedTreeMergeDriverMergesNonOverlappingText(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("alpha\nmiddle\nomega\n"),
		[]byte("alpha current\nmiddle\nomega\n"),
		[]byte("alpha\nmiddle\nomega other\n"),
		"", "",
	)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.NoError(err, string(out))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Contains(string(contents), "alpha current")
	assert.Contains(string(contents), "omega other")
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
}

func TestUntrustedTreeMergeDriverWritesDiff3ConflictMarkers(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("before\nbase\nafter\n"),
		[]byte("before\ncurrent\nafter\n"),
		[]byte("before\nother\nafter\n"),
		"", "",
	)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Equal("UU payload",
		lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal(
		"before\n<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\nafter\n",
		string(contents),
	)
}

func TestUntrustedTreeMergeDriverDoesNotEvaluateGitLabels(t *testing.T) {
	t.Run("merge branch label", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		markers := []string{"branch-dollar-marker", "branch-backtick-marker"}
		fixture := newMergeDriverFixture(t,
			[]byte("base\n"),
			[]byte("current\n"),
			[]byte("other\n"),
			"",
			"other-$(touch${IFS}branch-dollar-marker)-"+
				"`touch${IFS}branch-backtick-marker`",
		)

		cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
		out, err := cmd.CombinedOutput()

		require.Error(err, string(out))
		assert.Equal("UU payload",
			lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
		for _, marker := range markers {
			assert.NoFileExists(filepath.Join(fixture.worktree, marker))
		}
		contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
		require.NoError(err)
		assert.Equal(
			"<<<<<<< current\ncurrent\n||||||| base\nbase\n=======\nother\n>>>>>>> other\n",
			string(contents),
		)
	})

	t.Run("rebase commit subject label", func(t *testing.T) {
		require := require.New(t)
		assert := assert.New(t)
		markers := []string{"subject-dollar-marker", "subject-backtick-marker"}
		fixture := newMergeDriverFixture(t,
			[]byte("base\n"),
			[]byte("current\n"),
			[]byte("other\n"),
			"current $(touch${IFS}subject-dollar-marker) "+
				"`touch${IFS}subject-backtick-marker`",
			"",
		)

		cmd := lifecycleGitCommand(t, fixture.worktree, "rebase", fixture.otherRef)
		out, err := cmd.CombinedOutput()

		require.Error(err, string(out))
		assert.Equal("UU payload",
			lifecycleGit(t, fixture.worktree, "status", "--short", "--", "payload"))
		for _, marker := range markers {
			assert.NoFileExists(filepath.Join(fixture.worktree, marker))
		}
		contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
		require.NoError(err)
		for _, want := range []string{
			"<<<<<<< current\n",
			"||||||| base\n",
			"=======\n",
			">>>>>>> other\n",
			"base\n",
			"current\n",
			"other\n",
		} {
			assert.Contains(string(contents), want)
		}
		assert.NotContains(string(contents), "subject-dollar-marker")
		assert.NotContains(string(contents), "subject-backtick-marker")
	})
}

func TestUntrustedTreeMergeDriverFailsWhenResolvedGitDisappears(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	gitPath, err := exec.LookPath("git")
	require.NoError(err)
	gitContents, err := os.ReadFile(gitPath)
	require.NoError(err)
	copyName := "git"
	if runtime.GOOS == "windows" {
		copyName += filepath.Ext(gitPath)
	}
	gitCopy := filepath.Join(t.TempDir(), copyName)
	require.NoError(os.WriteFile(gitCopy, gitContents, 0o755))
	require.NoError(os.Chmod(gitCopy, 0o755))
	lifecycleGit(t, fixture.worktree, "config", "--worktree",
		"merge.owned.driver", safeMergeDriverCommand(gitCopy))
	require.NoError(os.Remove(gitCopy))

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", fixture.otherRef)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	assert.Empty(lifecycleGit(t, fixture.worktree, "status", "--short"))
	assert.Empty(lifecycleGit(t, fixture.worktree, "ls-files", "--unmerged"))
	contents, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Equal("current\n", string(contents))
}

func TestUntrustedTreeMergeDriverKeepsBinaryConflictLocal(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	fixture := newMergeDriverFixture(t,
		[]byte("base\n"), []byte("current\n"), []byte("other\n"), "", "",
	)

	otherWorktree := filepath.Join(t.TempDir(), "other")
	lifecycleGit(t, fixture.worktree, "worktree", "add", "--detach",
		otherWorktree, fixture.otherRef)
	for _, worktree := range []string{fixture.worktree, otherWorktree} {
		require.NoError(os.WriteFile(
			filepath.Join(worktree, ".gitattributes"),
			[]byte("payload merge=owned\nbinary.dat merge=owned\n"), 0o644,
		))
	}
	currentBinary := []byte("\x00current\n")
	otherBinary := []byte("\x00other\n")
	require.NoError(os.WriteFile(
		filepath.Join(fixture.worktree, "binary.dat"), currentBinary, 0o644,
	))
	lifecycleGit(t, fixture.worktree, "add", ".gitattributes", "binary.dat")
	lifecycleGit(t, fixture.worktree, "commit", "-qm", "current binary")
	require.NoError(os.WriteFile(
		filepath.Join(otherWorktree, "binary.dat"), otherBinary, 0o644,
	))
	lifecycleGit(t, otherWorktree, "add", ".gitattributes", "binary.dat")
	lifecycleGit(t, otherWorktree, "commit", "-qm", "other binary")
	otherCommit := lifecycleGit(t, otherWorktree, "rev-parse", "HEAD")
	lifecycleGit(t, fixture.worktree, "worktree", "remove", "--force", otherWorktree)

	cmd := lifecycleGitCommand(t, fixture.worktree, "merge", otherCommit)
	out, err := cmd.CombinedOutput()

	require.Error(err, string(out))
	status := strings.Split(
		lifecycleGit(t, fixture.worktree, "status", "--short"), "\n",
	)
	assert.ElementsMatch([]string{"UU binary.dat", "UU payload"}, status)
	binary, err := os.ReadFile(filepath.Join(fixture.worktree, "binary.dat"))
	require.NoError(err)
	assert.Equal(currentBinary, binary)
	assert.NotContains(string(binary), "<<<<<<<")
	payload, err := os.ReadFile(filepath.Join(fixture.worktree, "payload"))
	require.NoError(err)
	assert.Contains(string(payload), "<<<<<<< current")
	assert.Contains(string(payload), "||||||| base")
	assert.Contains(string(payload), ">>>>>>> other")
	// Git merge-file names its temporary %A file in this expected diagnostic.
	assert.Contains(string(out), "Cannot merge binary files: ")
}
