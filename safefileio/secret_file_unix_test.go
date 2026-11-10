//go:build darwin || linux

package safefileio

import (
	"context"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"hegel.dev/go/hegel"
)

type secretInfo struct {
	mode fs.FileMode
	uid  uint32
}

func (i secretInfo) Mode() fs.FileMode  { return i.mode }
func (i secretInfo) Sys() any           { return &syscall.Stat_t{Uid: i.uid} }
func (i secretInfo) IsDir() bool        { return i.mode.IsDir() }
func (i secretInfo) Name() string       { return "fixture" }
func (i secretInfo) Size() int64        { return 0 }
func (i secretInfo) ModTime() time.Time { return time.Time{} }

func TestSecretOwnerModePolicyProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		uid := hegel.Draw(ht, hegel.Integers[uint32](0, ^uint32(0)))
		// Exercise both trusted identities as well as the full foreign-UID domain.
		switch hegel.Draw(ht, hegel.Integers(0, 2)) {
		case 0:
			uid = 0
		case 1:
			uid = 1
		}
		mode := fs.FileMode(hegel.Draw(ht, hegel.Integers[uint32](0, ^uint32(0))))
		if hegel.Draw(ht, hegel.Booleans()) {
			mode = fs.FileMode(hegel.Draw(ht, hegel.Integers[uint32](0, 0o777)))
		}
		info := secretInfo{mode: mode, uid: uid}
		err := validateSecretInfo(info, 1)
		allowed := mode.IsRegular() && (uid == 0 && mode.Perm()&0o022 == 0 || uid == 1 && (mode.Perm() == 0o400 || mode.Perm() == 0o600))
		assert.Equal(ht, allowed, err == nil)
	}, hegel.WithTestCases(2000))
}

func TestParentMetadataPolicyProperty(t *testing.T) {
	hegel.Test(t, func(ht *hegel.T) {
		current := uint32(os.Geteuid())
		owner := hegel.Draw(ht, hegel.OneOf(hegel.SampledFrom([]uint32{0, current}), hegel.Integers[uint32](0, ^uint32(0))))
		childOwner := hegel.Draw(ht, hegel.OneOf(hegel.SampledFrom([]uint32{0, current}), hegel.Integers[uint32](0, ^uint32(0))))
		mode := fs.FileMode(hegel.Draw(ht, hegel.Integers[uint32](0, 0o777)))
		if hegel.Draw(ht, hegel.Booleans()) {
			mode |= os.ModeDir
		}
		if hegel.Draw(ht, hegel.Booleans()) {
			mode |= os.ModeSticky
		}
		parent := secretInfo{mode: mode, uid: owner}
		child := secretInfo{uid: childOwner}
		trustedOwner := owner == 0 || owner == current
		trustedChild := childOwner == 0 || childOwner == current
		allowed := mode.IsDir() && trustedOwner && (mode.Perm()&0o022 == 0 || mode&os.ModeSticky != 0 && trustedChild)
		assert.Equal(ht, allowed, requireUnsharedParent(parent, child) == nil)
	}, hegel.WithTestCases(2000))
}

func TestParentStickyRequiresTrustedChild(t *testing.T) {
	parent := secretInfo{mode: os.ModeDir | os.ModeSticky | 0o777, uid: 0}
	require.NoError(t, requireUnsharedParent(parent, secretInfo{uid: uint32(os.Geteuid())}))
	require.Error(t, requireUnsharedParent(parent, secretInfo{uid: uint32(os.Geteuid() + 1)}))
	require.Error(t, requireUnsharedParent(secretInfo{mode: os.ModeDir | 0o777, uid: 0}, secretInfo{uid: 0}))
}

func TestSecretOpenCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("secret"), 0o600))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	file, err := OpenSecretFile(ctx, path)
	require.ErrorIs(t, err, context.Canceled)
	assert.Nil(t, file)
}

func TestOpenSecretFileTraversesExecutableOnlyParentProperty(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "search-only")
	require.NoError(t, os.Mkdir(parent, 0o700))
	path := filepath.Join(parent, "token")
	want := []byte("secret")
	require.NoError(t, os.WriteFile(path, want, 0o600))
	defer func() { _ = os.Chmod(parent, 0o700) }()

	hegel.Test(t, func(ht *hegel.T) {
		mode := fs.FileMode(hegel.Draw(ht, hegel.Integers[uint32](0, 0o777)))
		require.NoError(ht, os.Chmod(parent, mode))
		file, err := OpenSecretFile(t.Context(), path)
		if (os.Geteuid() != 0 && mode&0o100 == 0) || mode.Perm()&0o022 != 0 {
			require.Error(ht, err)
			assert.Nil(ht, file)
			return
		}
		require.NoError(ht, err)
		got, err := io.ReadAll(file)
		require.NoError(ht, err)
		assert.Equal(ht, want, got)
		require.NoError(ht, file.Close())
	}, hegel.WithTestCases(2000))
}

func TestOpenSecretFileDoesNotNormalizeAwayUnsearchableDotDotComponent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse a directory without search permission")
	}
	root := t.TempDir()
	blocked := filepath.Join(root, "blocked")
	require.NoError(t, os.Mkdir(blocked, 0o700))
	defer func() { _ = os.Chmod(blocked, 0o700) }()
	token := filepath.Join(root, "token")
	require.NoError(t, os.WriteFile(token, []byte("secret"), 0o600))
	require.NoError(t, os.Chmod(blocked, 0o600))
	path := blocked + string(filepath.Separator) + ".." + string(filepath.Separator) + "token"

	file, err := OpenSecretFile(t.Context(), path)
	require.Error(t, err)
	assert.Nil(t, file)
}
