//go:build darwin || linux

package secretref_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/secretref"
	"pgregory.net/rapid"
)

func TestReadOnlyAndMountedLinks(t *testing.T) {
	dir := privateTempDir(t)
	version := filepath.Join(dir, "..version")
	require.NoError(t, os.Mkdir(version, 0o700))
	key := filepath.Join(version, "token")
	writePrivateFile(t, key, "mounted\r\n")
	require.NoError(t, os.Chmod(key, 0o400))
	require.NoError(t, os.Symlink("..version", filepath.Join(dir, "..data")))
	link := filepath.Join(dir, "token")
	require.NoError(t, os.Symlink("..data/token", link))
	got, err := (secretref.Ref{File: link}).ResolveContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "mounted", got.Value)
}

func TestHomeAndAbsolutePathAgreementProperty(t *testing.T) {
	home := privateTempDir(t)
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "target", "inner"), 0o700))
	require.NoError(t, os.Symlink("target/inner", filepath.Join(home, "link")))
	rapid.Check(t, func(ht *rapid.T) {
		value := "token:" + rapid.String().Draw(ht, "text")
		require.NoError(ht, os.WriteFile(filepath.Join(home, "target", "token"), []byte(value), 0o600))
		suffix := rapid.SampledFrom([]string{"link/../token", "target//./token", "target/inner/../token", "target/token/", "missing/../target/token"}).Draw(ht, "suffix")
		absolute, absErr := (secretref.Ref{File: home + "/" + suffix}).ResolveContext(t.Context())
		relative, relErr := (secretref.Ref{File: "~/" + suffix}).ResolveContext(t.Context())
		assert.Equal(ht, absErr == nil, relErr == nil)
		assert.Equal(ht, absolute.Value, relative.Value)
	})
}

func TestFileTransformProperty(t *testing.T) {
	path := filepath.Join(privateTempDir(t), "token")
	rapid.Check(t, func(ht *rapid.T) {
		data := rapid.SliceOf(rapid.Byte()).Draw(ht, "bytes")
		require.NoError(ht, os.WriteFile(path, data, 0o600))
		got, err := (secretref.Ref{File: path}).ResolveContext(t.Context())
		end := len(data)
		for end > 0 && (data[end-1] == '\r' || data[end-1] == '\n') {
			end--
		}
		want := string(data[:end])
		if len(data) > 64<<10 || strings.TrimSpace(want) == "" || strings.ContainsRune(want, 0) {
			require.Error(ht, err)
			assert.Equal(ht, secretref.Secret{}, got)
		} else {
			require.NoError(ht, err)
			assert.Equal(ht, want, got.Value)
		}
	})
}

func TestUnsafeAncestorAndLoopRejected(t *testing.T) {
	dir := privateTempDir(t)
	unsafe := filepath.Join(dir, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o700))
	key := filepath.Join(unsafe, "token")
	writePrivateFile(t, key, "secret")
	require.NoError(t, os.Chmod(unsafe, 0o777))
	_, err := (secretref.Ref{File: key}).Resolve()
	require.Error(t, err)
	require.NoError(t, os.Chmod(unsafe, 0o700))
	link := filepath.Join(dir, "loop")
	require.NoError(t, os.Symlink("loop", link))
	_, err = (secretref.Ref{File: link}).Resolve()
	require.Error(t, err)
	// Lexically cleaning would erase unsafe before checking its trust.
	require.NoError(t, os.Chmod(unsafe, 0o777))
	writePrivateFile(t, filepath.Join(dir, "token"), "secret")
	_, err = (secretref.Ref{File: unsafe + "/../token"}).Resolve()
	require.Error(t, err)
}

func TestHomeExpansionKeepsTraversedComponents(t *testing.T) {
	home := privateTempDir(t)
	t.Setenv("HOME", home)
	unsafe := filepath.Join(home, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o700))
	require.NoError(t, os.Chmod(unsafe, 0o777))
	writePrivateFile(t, filepath.Join(home, "token"), "secret")
	_, err := (secretref.Ref{File: "~/unsafe/../token"}).ResolveContext(t.Context())
	require.Error(t, err)
}

func TestHomeExpansionPreservesSymlinkDotDotMeaning(t *testing.T) {
	home := privateTempDir(t)
	t.Setenv("HOME", home)
	target := filepath.Join(home, "target", "inner")
	require.NoError(t, os.MkdirAll(target, 0o700))
	writePrivateFile(t, filepath.Join(home, "target", "token"), "target-token")
	writePrivateFile(t, filepath.Join(home, "token"), "home-token")
	require.NoError(t, os.Symlink(target, filepath.Join(home, "link")))
	got, err := (secretref.Ref{File: "~/link/../token"}).ResolveContext(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "target-token", got.Value)
}
