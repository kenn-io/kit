package backup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
)

func TestCaptureExtrasDeletionsAndConfig(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "deletions", "sub"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "a.json"), []byte(`{"a":1}`), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "deletions", "sub", "b.json"), []byte(`{"b":2}`), 0o600))
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("x = 1\n"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	treeID, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.True(hasTree)

	packs, entries, err := appender.Finish()
	require.NoError(err)
	require.NotEmpty(packs)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	require.Len(tree.Entries, 3)
	assert.Equal("config.toml", tree.Entries[0].Path)
	assert.Equal("deletions/a.json", tree.Entries[1].Path)
	assert.Equal("deletions/sub/b.json", tree.Entries[2].Path)
	for _, e := range tree.Entries {
		blob, parseErr := pack.ParseBlobID(e.Blob)
		require.NoError(parseErr)
		content, readErr := r.ReadBlob(known, blob, nil, testPackExt)
		require.NoError(readErr)
		assert.Equal(e.Size, int64(len(content)), e.Path)
	}
}

func TestCaptureExtrasEmpty(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, hasTree, err := CaptureExtras(ExtrasOptions{DataDir: t.TempDir()}, appender)
	require.NoError(err)
	require.False(hasTree)
}

func TestCaptureExtrasTokensWithoutDataDir(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	// A client_secret file in the process's cwd must not be globbed (and the
	// absent data root must not be dereferenced) when no DataDir is set.
	cwd := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(cwd, "client_secret_x.json"), []byte("{}"), 0o600))
	t.Chdir(cwd)
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, hasTree, err := CaptureExtras(ExtrasOptions{
		IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.False(hasTree)
}

func TestCaptureExtrasTokensGuard(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	dataDir := t.TempDir()
	require.NoError(os.MkdirAll(filepath.Join(dataDir, "tokens"), 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "tokens", "t.json"), []byte("{}"), 0o600))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "client_secret_web.json"), []byte("{}"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, _, err := CaptureExtras(ExtrasOptions{DataDir: dataDir, IncludeTokens: true}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "--include-tokens")

	// --include-config is just as sensitive: config.toml carries API keys, so
	// it fires the same guard and names the flag it tripped on.
	cfgPath := filepath.Join(dataDir, "config.toml")
	require.NoError(os.WriteFile(cfgPath, []byte("[server]\napi_key = \"secret\"\n"), 0o600))
	_, _, err = CaptureExtras(ExtrasOptions{DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true}, appender)
	require.ErrorContains(err, "encrypted repository")
	require.ErrorContains(err, "--include-config")

	_, _, err = CaptureExtras(ExtrasOptions{
		DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)

	treeID, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.True(hasTree)

	_, entries, err := appender.Finish()
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	var paths []string
	for _, e := range tree.Entries {
		paths = append(paths, e.Path)
	}
	require.Equal([]string{"client_secret_web.json", "tokens/t.json"}, paths)
}

// TestCaptureExtrasConfigSymlinkEscapeRefused pins the confined read for the
// config file: config.toml is read through a root at its own directory, so a
// symlink swapped in at ConfigPath that points outside that directory is
// refused rather than followed to an arbitrary host file. Before the fix the
// plain os.ReadFile followed it and embedded the target's bytes.
func TestCaptureExtrasConfigSymlinkEscapeRefused(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()
	secret := filepath.Join(outsideDir, "secret.txt")
	require.NoError(os.WriteFile(secret, []byte("TOP SECRET config"), 0o600))

	cfgPath := filepath.Join(dataDir, "config.toml")
	if err := os.Symlink(secret, cfgPath); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, _, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, ConfigPath: cfgPath, IncludeConfig: true, AllowPlaintextSecrets: true,
	}, appender)
	require.Error(err)

	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "TOP SECRET config")
	}
}

func TestCaptureExtrasRejectsSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	// Create a target file outside the data dir
	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	// Create a symlink inside deletions pointing to the outside file
	deletionsDir := filepath.Join(dataDir, "deletions")
	require.NoError(os.MkdirAll(deletionsDir, 0o700))
	symlinkPath := filepath.Join(deletionsDir, "link.txt")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		// Skip if symlinks are not supported on this platform
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	_, _, err = CaptureExtras(ExtrasOptions{DataDir: dataDir}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "deletions/link.txt")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "secret content")
	}
}

// TestCaptureExtrasRejectsGlobbedSymlinks pins the fix extending the walk's
// symlink rejection to filepath.Glob's client_secret*.json results, which
// never went through addDir's filepath.WalkDir callback and so bypassed the
// os.ReadFile-follows-symlinks hazard TestCaptureExtrasRejectsSymlinks
// covers for the deletions/tokens walk.
func TestCaptureExtrasRejectsGlobbedSymlinks(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	r := initTestRepo(t)

	dataDir := t.TempDir()
	outsideDir := t.TempDir()

	targetPath := filepath.Join(outsideDir, "sensitive.txt")
	require.NoError(os.WriteFile(targetPath, []byte("secret content"), 0o600))

	symlinkPath := filepath.Join(dataDir, "client_secret_evil.json")
	err := os.Symlink(targetPath, symlinkPath)
	if err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	defer appender.Abort()
	_, _, err = CaptureExtras(ExtrasOptions{
		DataDir: dataDir, IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.Error(err)
	assert.Contains(err.Error(), "client_secret_evil.json")
	assert.Contains(err.Error(), "not a regular file")

	// Verify the symlink target content is not embedded.
	_, entries, errFinish := appender.Finish()
	require.NoError(errFinish)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	for _, e := range entries {
		content, _ := r.ReadBlob(known, e.Blob, nil, testPackExt)
		assert.NotContains(string(content), "secret content")
	}
}

// TestCaptureExtrasTokensDataDirWithGlobMetacharacters pins that client-secret
// capture works when DataDir's path contains glob metacharacters. The old
// filepath.Glob(filepath.Join(DataDir, "client_secret*.json")) treated the
// bracket in "data[1]" as a character class, so the join never matched and the
// secret was silently dropped from the backup. Matching by basename through the
// confined root captures it.
func TestCaptureExtrasTokensDataDirWithGlobMetacharacters(t *testing.T) {
	require := require.New(t)
	r := initTestRepo(t)
	// "data[1]" is a legal directory name on every platform but a glob
	// character class as a pattern prefix.
	dataDir := filepath.Join(t.TempDir(), "data[1]")
	require.NoError(os.MkdirAll(dataDir, 0o700))
	require.NoError(os.WriteFile(filepath.Join(dataDir, "client_secret_web.json"), []byte("{}"), 0o600))

	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, testPackExt)
	treeID, hasTree, err := CaptureExtras(ExtrasOptions{
		DataDir: dataDir, IncludeTokens: true, AllowPlaintextSecrets: true,
	}, appender)
	require.NoError(err)
	require.True(hasTree)

	_, entries, err := appender.Finish()
	require.NoError(err)
	known := map[pack.BlobID]IndexEntry{}
	for _, e := range entries {
		known[e.Blob] = e
	}
	treeData, err := r.ReadBlob(known, treeID, nil, testPackExt)
	require.NoError(err)
	var tree ExtrasTree
	require.NoError(json.Unmarshal(treeData, &tree))
	var paths []string
	for _, e := range tree.Entries {
		paths = append(paths, e.Path)
	}
	require.Equal([]string{"client_secret_web.json"}, paths)
}
