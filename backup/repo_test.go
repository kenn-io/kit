package backup

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInitCreatesLayout(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)

	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	for _, dir := range []string{"snapshots", "packs", "indexes", "locks", "staging", "keys"} {
		info, statErr := os.Stat(filepath.Join(root, dir))
		require.NoError(statErr, dir)
		assert.True(info.IsDir(), dir)
	}
	cfg := r.Config()
	assert.Len(cfg.RepoID, 36)
	assert.Equal(FormatVersion, cfg.FormatVersion)
	assert.Equal(MinReaderVersion, cfg.MinReaderVersion)
	assert.Equal("none", cfg.Encryption)
	assert.NotEmpty(cfg.CreatedAt)
}

func TestInitRefusesExistingRepo(t *testing.T) {
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(t, err)
	_, err = Init(root)
	require.Error(t, err)
}

func TestOpenRoundTrip(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	created, err := Init(root)
	require.NoError(err)

	opened, err := Open(root)
	require.NoError(err)
	assert.Equal(t, created.Config().RepoID, opened.Config().RepoID)
}

func TestOpenRejectsFutureRepo(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(err)

	data, err := os.ReadFile(filepath.Join(root, "config.toml"))
	require.NoError(err)

	munged := replaceInTOML(string(data), "min_reader_version", "99")
	require.NoError(os.WriteFile(filepath.Join(root, "config.toml"), []byte(
		munged), 0o600))

	_, err = Open(root)
	require.ErrorContains(err, "upgrade the reader")
}

func replaceInTOML(content, key, value string) string {
	re := regexp.MustCompile(key + ` = \d+`)
	return re.ReplaceAllString(content, key+` = `+value)
}

// TestOpenRejectsNonCanonicalRepoID pins that Open refuses a config.toml
// whose repo_id is not the canonical generated UUID: the ID is joined into
// cache filenames, so a tampered value carrying path separators could
// otherwise address files outside the cache directory.
func TestOpenRejectsNonCanonicalRepoID(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	_, err := Init(root)
	require.NoError(err)

	cfgPath := filepath.Join(root, "config.toml")
	data, err := os.ReadFile(cfgPath)
	require.NoError(err)
	re := regexp.MustCompile(`repo_id = "[^"]*"`)

	for _, id := range []string{
		`../../../home/user/.ssh/authorized_keys`,
		`sub/dir`, ``, `AAAAAAAA-0000-4000-8000-000000000000`,
	} {
		munged := re.ReplaceAllString(string(data), `repo_id = "`+id+`"`)
		require.NoError(os.WriteFile(cfgPath, []byte(munged), 0o600))
		_, err = Open(root)
		require.ErrorContains(err, "not the canonical UUID", "repo_id %q", id)
	}
}

func TestCleanStaging(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	debris := filepath.Join(root, "staging", "leftover.tmp")
	require.NoError(os.WriteFile(debris, []byte("x"), 0o600))
	require.NoError(r.CleanStaging())
	_, statErr := os.Stat(debris)
	require.True(os.IsNotExist(statErr))
}

func TestWriteFileAtomic(t *testing.T) {
	require := require.New(t)
	root := filepath.Join(t.TempDir(), "repo")
	r, err := Init(root)
	require.NoError(err)

	require.NoError(writeFileAtomic(r, filepath.Join("indexes", "x.mvidx"), []byte("hello")))
	got, err := os.ReadFile(r.Path("indexes", "x.mvidx"))
	require.NoError(err)
	assert.Equal(t, []byte("hello"), got)
	entries, err := os.ReadDir(r.Path("staging"))
	require.NoError(err)
	assert.Empty(t, entries)
}

// TestCleanStagingRefusesSymlinkedStaging pins that staging cleanup never
// follows a planted symlink: if another principal replaces the staging
// directory with a link, cleanup must refuse rather than delete entries in
// the link's target as the calling user.
func TestCleanStagingRefusesSymlinkedStaging(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)

	victimDir := t.TempDir()
	victim := filepath.Join(victimDir, "precious.txt")
	require.NoError(os.WriteFile(victim, []byte("do not delete"), 0o600))

	staging := r.Path("staging")
	require.NoError(os.RemoveAll(staging))
	if err := os.Symlink(victimDir, staging); err != nil {
		// Windows restricts symlink creation to elevated or developer-mode
		// users; the guard under test is platform-independent.
		t.Skip("symlinks not supported on this platform")
	}

	err := r.CleanStaging()
	require.ErrorContains(err, "not a directory")
	_, statErr := os.Stat(victim)
	require.NoError(statErr, "the symlink target's contents must be untouched")
}
