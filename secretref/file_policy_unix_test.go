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
	"hegel.dev/go/hegel"
)

func TestReadOnlyAndMountedLinks(t *testing.T) {
	dir := t.TempDir()
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	require.NoError(t, os.MkdirAll(filepath.Join(home, "target", "inner"), 0o700))
	require.NoError(t, os.Symlink("target/inner", filepath.Join(home, "link")))
	hegel.Test(t, func(ht *hegel.T) {
		value := "token:" + hegel.Draw(ht, hegel.Text())
		require.NoError(ht, os.WriteFile(filepath.Join(home, "target", "token"), []byte(value), 0o600))
		suffix := hegel.Draw(ht, hegel.SampledFrom([]string{"link/../token", "target//./token", "target/inner/../token", "target/token/", "missing/../target/token"}))
		absolute, absErr := (secretref.Ref{File: home + "/" + suffix}).ResolveContext(t.Context())
		relative, relErr := (secretref.Ref{File: "~/" + suffix}).ResolveContext(t.Context())
		assert.Equal(ht, absErr == nil, relErr == nil)
		assert.Equal(ht, absolute.Value, relative.Value)
	}, hegel.WithTestCases(2000))
}

func TestFileTransformProperty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	hegel.Test(t, func(ht *hegel.T) {
		data := hegel.Draw(ht, hegel.Binary(0, -1))
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
	}, hegel.WithTestCases(2000))
}

func TestUnsafeAncestorAndLoopRejected(t *testing.T) {
	dir := t.TempDir()
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
	home := t.TempDir()
	t.Setenv("HOME", home)
	unsafe := filepath.Join(home, "unsafe")
	require.NoError(t, os.Mkdir(unsafe, 0o700))
	require.NoError(t, os.Chmod(unsafe, 0o777))
	writePrivateFile(t, filepath.Join(home, "token"), "secret")
	_, err := (secretref.Ref{File: "~/unsafe/../token"}).ResolveContext(t.Context())
	require.Error(t, err)
}

func TestHomeExpansionPreservesSymlinkDotDotMeaning(t *testing.T) {
	home := t.TempDir()
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
