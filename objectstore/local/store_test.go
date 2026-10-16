package local

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/atomicfile"
	"go.kenn.io/kit/objectstore"
)

func TestPublishImmutable(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	original := []byte("original")
	receipt, err := store.Publish(t.Context(), "nested/key", bytes.NewReader(original), 8, sha256.Sum256(original))
	require.NoError(t, err)
	assert.Equal(t, "nested/key", receipt.Object.Key)
	assert.Equal(t, int64(8), receipt.Object.Size)
	assert.Empty(t, receipt.Object.Revision)
	assert.Equal(t, "readback-sha256", receipt.Verification)
	assert.Equal(t, "UTC", receipt.Object.LastModified.Location().String())
	assert.Equal(t, []objectstore.Checksum{{Algorithm: "sha256", Value: "BoLF8gdvCZw0z90VqeBjhJ7UN6SWd+b8xbQZjHZXW+U=", Scope: "full"}}, receipt.Object.Checksums)
	replacement := []byte("different")
	failed, err := store.Publish(t.Context(), "nested/key", bytes.NewReader(replacement), 9, sha256.Sum256(replacement))
	require.ErrorIs(t, err, objectstore.ErrExists)
	assert.Zero(t, failed)
	stored, err := os.ReadFile(filepath.Join(root, "nested", "key"))
	require.NoError(t, err)
	assert.Equal(t, original, stored)
	entries, err := os.ReadDir(filepath.Join(root, "nested"))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "key", entries[0].Name())
}

func TestPublishIdenticalRetry(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	source := []byte("original")
	digest := sha256.Sum256(source)
	_, err = store.Publish(t.Context(), "key", bytes.NewReader(source), 8, digest)
	require.NoError(t, err)
	before, err := os.Stat(filepath.Join(root, "key"))
	require.NoError(t, err)
	receipt, err := store.Publish(t.Context(), "key", bytes.NewReader(source), 8, digest)
	require.NoError(t, err)
	assert.Equal(t, digest, receipt.SHA256)
	after, err := os.Stat(filepath.Join(root, "key"))
	require.NoError(t, err)
	assert.True(t, os.SameFile(before, after))
}

func TestPublishWrongDigest(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	for _, key := range []string{"new", "existing"} {
		if key == "existing" {
			require.NoError(t, os.WriteFile(filepath.Join(root, key), []byte("kept"), 0o600))
		}
		for _, size := range []int64{-1, 7, 8, 9} {
			digest := sha256.Sum256([]byte("original"))
			if size == 8 {
				digest = [32]byte{}
			}
			receipt, err := store.Publish(t.Context(), key, bytes.NewReader([]byte("original")), size, digest)
			require.ErrorIs(t, err, objectstore.ErrChecksumMismatch)
			assert.Zero(t, receipt)
		}
	}
	_, err = os.Stat(filepath.Join(root, "new"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	stored, err := os.ReadFile(filepath.Join(root, "existing"))
	require.NoError(t, err)
	assert.Equal(t, []byte("kept"), stored)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Len(t, entries, 1)
}

func TestPublishUncertainDurability(t *testing.T) {
	root := t.TempDir()
	// New resolves links and platform-specific path spelling before syncing.
	root, err := filepath.EvalSymlinks(root)
	require.NoError(t, err)
	store, err := New(root)
	require.NoError(t, err)
	syncErr := errors.New("directory sync failed")
	var synced []string
	store.syncDir = func(dir string) error {
		synced = append(synced, dir)
		if dir == root {
			return syncErr
		}
		return atomicfile.SyncDir(dir)
	}
	source := []byte("original")
	digest := sha256.Sum256(source)
	receipt, err := store.Publish(t.Context(), "nested/key", bytes.NewReader(source), 8, digest)
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, syncErr)
	assert.Zero(t, receipt)
	stored, err := os.ReadFile(filepath.Join(root, "nested", "key"))
	require.NoError(t, err)
	assert.Equal(t, source, stored)
	assert.Equal(t, []string{filepath.Join(root, "nested"), root}, synced)
	// A match alone cannot resolve the failed sync.
	receipt, err = store.Publish(t.Context(), "nested/key", bytes.NewReader(source), 8, digest)
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	store.syncDir = atomicfile.SyncDir
	receipt, err = store.Publish(t.Context(), "nested/key", bytes.NewReader(source), 8, digest)
	require.NoError(t, err)
	assert.Equal(t, digest, receipt.SHA256)
}

func TestRangeErrors(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(root, "key"), []byte("original"), 0o600))
	reader, err := store.OpenRange(t.Context(), "key", "", 1, 3)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Equal(t, []byte("rig"), data)
	require.NoError(t, reader.Close())
	for _, interval := range [][2]int64{{-1, 1}, {0, -1}, {7, 2}, {9, 0}, {1, 1<<63 - 1}} {
		reader, err = store.OpenRange(t.Context(), "key", "", interval[0], interval[1])
		require.Error(t, err)
		assert.Nil(t, reader)
	}
	reader, err = store.OpenRange(t.Context(), "missing", "", 0, 1)
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	assert.Nil(t, reader)
	reader, err = store.OpenRange(t.Context(), "key", "", 0, 8)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(filepath.Join(root, "key"), 2))
	_, err = io.ReadAll(reader)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	require.NoError(t, reader.Close())
	ctx, cancel := context.WithCancel(t.Context())
	reader, err = store.OpenRange(ctx, "key", "", 0, 2)
	require.NoError(t, err)
	cancel()
	_, err = io.ReadAll(reader)
	require.ErrorIs(t, err, context.Canceled)
	require.NoError(t, reader.Close())
}

func TestListPages(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	for _, key := range []string{"a", "b", "c", "foreign.name", "prefix/z", "prefix.txt"} {
		path := filepath.Join(root, filepath.FromSlash(key))
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte(key), 0o600))
	}
	var keys []string
	token := ""
	for {
		page, err := store.ListPage(t.Context(), "", token, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page.Objects), 2)
		for _, item := range page.Objects {
			keys = append(keys, item.Key)
		}
		if page.NextToken == "" {
			break
		}
		require.NotEqual(t, token, page.NextToken)
		token = page.NextToken
	}
	assert.Equal(t, []string{"a", "b", "c", "foreign.name", "prefix.txt", "prefix/z"}, keys)
	page, err := store.ListPage(t.Context(), "prefix/", "", 1)
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	assert.Equal(t, "prefix/z", page.Objects[0].Key)
	assert.Empty(t, page.NextToken)
	_, err = store.ListPage(t.Context(), "", "garbage", 2)
	require.Error(t, err)
	_, err = store.ListPage(t.Context(), "", "", 0)
	require.Error(t, err)
}

func TestInspectDeleteAndCancellation(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	_, err = store.Inspect(t.Context(), "missing")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
	require.NoError(t, store.Delete(t.Context(), "missing", ""))
	require.NoError(t, os.WriteFile(filepath.Join(root, "key"), []byte("original"), 0o600))
	meta, err := store.Inspect(t.Context(), "key")
	require.NoError(t, err)
	assert.Equal(t, int64(8), meta.Size)
	assert.Empty(t, meta.Revision)
	// Local cannot honor an opaque revision supplied by a caller.
	require.ErrorIs(t, store.Delete(t.Context(), "key", "revision"), errors.ErrUnsupported)
	_, err = store.OpenRange(t.Context(), "key", "revision", 0, 1)
	require.ErrorIs(t, err, errors.ErrUnsupported)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	receipt, err := store.Publish(ctx, "other", bytes.NewReader([]byte("x")), 1, sha256.Sum256([]byte("x")))
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, receipt)
	_, err = store.Inspect(ctx, "key")
	require.ErrorIs(t, err, context.Canceled)
	_, err = store.ListPage(ctx, "", "", 1)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, store.Delete(ctx, "key", ""), context.Canceled)
	require.NoError(t, store.Delete(t.Context(), "key", ""))
	_, err = store.Inspect(t.Context(), "key")
	require.ErrorIs(t, err, objectstore.ErrNotFound)
}

func TestPublishConcurrent(t *testing.T) {
	store, err := New(t.TempDir())
	require.NoError(t, err)
	type result struct {
		receipt objectstore.Receipt
		err     error
	}
	results := make(chan result, 8)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			data := []byte("original")
			receipt, err := store.Publish(t.Context(), "key", bytes.NewReader(data), 8, sha256.Sum256(data))
			results <- result{receipt: receipt, err: err}
		})
	}
	wg.Wait()
	close(results)
	for result := range results {
		require.NoError(t, result.err)
		assert.Equal(t, sha256.Sum256([]byte("original")), result.receipt.SHA256)
	}
}

func TestPublishSourceFailureAndOwnership(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	readErr := errors.New("source read failed")
	source := &failingSource{err: readErr}
	receipt, err := store.Publish(t.Context(), "key", source, 8, sha256.Sum256([]byte("original")))
	require.ErrorIs(t, err, readErr)
	assert.Zero(t, receipt)
	assert.False(t, source.closed)
	entries, err := os.ReadDir(root)
	require.NoError(t, err)
	assert.Empty(t, entries)
	source = &failingSource{Reader: bytes.NewReader([]byte("original"))}
	_, err = source.Seek(5, io.SeekStart)
	require.NoError(t, err)
	receipt, err = store.Publish(t.Context(), "key", source, 8, sha256.Sum256([]byte("original")))
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256([]byte("original")), receipt.SHA256)
	assert.False(t, source.closed)
}

type failingSource struct {
	*bytes.Reader
	err    error
	closed bool
}

func (s *failingSource) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	return s.Reader.Read(p)
}

func (s *failingSource) Seek(offset int64, whence int) (int64, error) {
	if s.Reader == nil {
		return 0, nil
	}
	return s.Reader.Seek(offset, whence)
}
func (s *failingSource) Close() error { s.closed = true; return nil }

func TestPublishCancellationAfterVisibility(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	store.syncDir = func(string) error { cancel(); return nil }
	data := []byte("original")
	receipt, err := store.Publish(ctx, "key", bytes.NewReader(data), 8, sha256.Sum256(data))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	require.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, receipt)
	stored, err := os.ReadFile(filepath.Join(root, "key"))
	require.NoError(t, err)
	assert.Equal(t, data, stored)
}

func TestRejectPathsAndLinks(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	for _, key := range []string{"../outside", "/absolute", `a\b`, "a/../b"} {
		_, err := store.Publish(t.Context(), key, bytes.NewReader(nil), 0, sha256.Sum256(nil))
		require.Error(t, err)
		_, err = store.Inspect(t.Context(), key)
		require.Error(t, err)
		_, err = store.OpenRange(t.Context(), key, "", 0, 0)
		require.Error(t, err)
		require.Error(t, store.Delete(t.Context(), key, ""))
	}
	outside := t.TempDir()
	err = os.Symlink(outside, filepath.Join(root, "link"))
	if err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	_, err = store.Publish(t.Context(), "link/key", bytes.NewReader(nil), 0, sha256.Sum256(nil))
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(outside, "key"))
	require.ErrorIs(t, err, fs.ErrNotExist)
	require.NoError(t, os.Symlink(filepath.Join(outside, "absent"), filepath.Join(root, "dangling")))
	receipt, err := store.Publish(t.Context(), "dangling", bytes.NewReader(nil), 0, sha256.Sum256(nil))
	require.ErrorIs(t, err, objectstore.ErrOutcomeUnknown)
	assert.Zero(t, receipt)
	_, err = store.Inspect(t.Context(), "dangling")
	require.Error(t, err)
	require.NotErrorIs(t, err, objectstore.ErrNotFound)
}

func TestEmptyObjectsAndRanges(t *testing.T) {
	store, err := New(t.TempDir())
	require.NoError(t, err)
	receipt, err := store.Publish(t.Context(), "empty", bytes.NewReader(nil), 0, sha256.Sum256(nil))
	require.NoError(t, err)
	assert.Equal(t, int64(0), receipt.Object.Size)
	reader, err := store.OpenRange(t.Context(), "empty", "", 0, 0)
	require.NoError(t, err)
	data, err := io.ReadAll(reader)
	require.NoError(t, err)
	assert.Empty(t, data)
	require.NoError(t, reader.Close())
}

func TestPublishReadOnlyRetry(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("read-only file sync is a Unix contract")
	}
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	data := []byte("original")
	require.NoError(t, os.WriteFile(filepath.Join(root, "key"), data, 0o400))
	receipt, err := store.Publish(t.Context(), "key", bytes.NewReader(data), 8, sha256.Sum256(data))
	require.NoError(t, err)
	assert.Equal(t, sha256.Sum256(data), receipt.SHA256)
	info, err := os.Stat(filepath.Join(root, "key"))
	require.NoError(t, err)
	assert.Equal(t, fs.FileMode(0o400), info.Mode().Perm())
}

func TestListPageAcrossDirectoryBatches(t *testing.T) {
	root := t.TempDir()
	store, err := New(root)
	require.NoError(t, err)
	for index := range 260 {
		key := fmt.Sprintf("%03d", index)
		require.NoError(t, os.WriteFile(filepath.Join(root, key), nil, 0o600))
	}
	page, err := store.ListPage(t.Context(), "", "", 300)
	require.NoError(t, err)
	require.Len(t, page.Objects, 260)
	assert.Equal(t, "000", page.Objects[0].Key)
	assert.Equal(t, "259", page.Objects[259].Key)
	assert.Empty(t, page.NextToken)
}

func TestPublishRejectsStagingKeys(t *testing.T) {
	for _, key := range []string{"final", "nested/final"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			store, err := New(root)
			require.NoError(t, err)
			otherStore, err := New(root)
			require.NoError(t, err)
			data := []byte("original")
			digest := sha256.Sum256(data)
			source := &stagingPausedSource{Reader: bytes.NewReader(data), ready: make(chan struct{}), resume: make(chan struct{})}
			type result struct {
				receipt objectstore.Receipt
				err     error
			}
			done := make(chan result, 1)
			resume := sync.OnceFunc(func() { close(source.resume) })
			var wg sync.WaitGroup
			wg.Go(func() {
				receipt, err := store.Publish(t.Context(), key, source, 8, digest)
				done <- result{receipt: receipt, err: err}
			})
			defer func() { resume(); wg.Wait() }()
			select {
			case <-source.ready:
			case result := <-done:
				require.NoError(t, result.err)
				require.FailNow(t, "publication finished before source reached its pause")
			}
			page, err := store.ListPage(t.Context(), "", "", 10)
			require.NoError(t, err)
			require.Len(t, page.Objects, 1)
			stagingKey := page.Objects[0].Key
			require.True(t, strings.HasPrefix(filepath.Base(stagingKey), ".objectstore-"))
			retryReceipt, retryErr := otherStore.Publish(t.Context(), stagingKey, bytes.NewReader(data), 8, digest)
			resume()
			original := <-done
			require.NoError(t, original.err)
			assert.Equal(t, digest, original.receipt.SHA256)
			stored, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(key)))
			require.NoError(t, err)
			assert.Equal(t, data, stored)
			require.Error(t, retryErr)
			assert.Zero(t, retryReceipt)
			page, err = store.ListPage(t.Context(), "", "", 10)
			require.NoError(t, err)
			require.Len(t, page.Objects, 1)
			assert.Equal(t, key, page.Objects[0].Key)
		})
	}
}

type stagingPausedSource struct {
	*bytes.Reader
	ready, resume chan struct{}
}

func (source *stagingPausedSource) Read(p []byte) (int, error) {
	if source.Len() == 0 {
		close(source.ready)
		<-source.resume
		return 0, io.EOF
	}
	return source.Reader.Read(p)
}

func TestReservedStagingPaths(t *testing.T) {
	for _, key := range []string{".objectstore-foreign", "nested/.objectstore-foreign", ".objectstore-dir/key", "nested/.objectstore-dir/key", "nested/.OBJECTSTORE-foreign"} {
		t.Run(key, func(t *testing.T) {
			root := t.TempDir()
			store, err := New(root)
			require.NoError(t, err)
			require.NoError(t, objectstore.ValidateKey(key))
			path := filepath.Join(root, filepath.FromSlash(key))
			require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
			data := []byte("foreign")
			require.NoError(t, os.WriteFile(path, data, 0o600))
			_, err = store.Inspect(t.Context(), key)
			require.Error(t, err)
			reader, err := store.OpenRange(t.Context(), key, "", 0, 7)
			if reader != nil {
				require.NoError(t, reader.Close())
			}
			require.Error(t, err)
			assert.Nil(t, reader)
			require.Error(t, store.Delete(t.Context(), key, ""))
			receipt, err := store.Publish(t.Context(), key, bytes.NewReader(data), 7, sha256.Sum256(data))
			require.Error(t, err)
			assert.Zero(t, receipt)
			stored, err := os.ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, data, stored)
			page, err := store.ListPage(t.Context(), "", "", 10)
			require.NoError(t, err)
			require.Len(t, page.Objects, 1)
			assert.Equal(t, key, page.Objects[0].Key)
		})
	}
}
