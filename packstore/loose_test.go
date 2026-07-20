package packstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestLooseWriteStreamsAndChecksExpectedMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("streamed-content-"), 32*1024)
	hash := hashForTest(content)
	size := int64(len(content))
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.Write(context.Background(), &boundedChunkReader{r: bytes.NewReader(content), max: 32 << 10}, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hash,
		ExpectedSize: size,
		SizeKnown:    true,
	})
	require.NoError(err)
	assert.Equal(hash, result.Hash)
	assert.Equal(size, result.Size)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(size, result.StoredSize)
	assert.True(result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(content, stored)
	assert.Empty(matchingFiles(t, filepath.Dir(result.Path), ".staging-"))
}

func TestLooseWriteBytesComputesIdentityBeforeSameDirectoryStaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("in-memory-content-"), 4096)
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	assert.Equal(hashForTest(content), result.Hash)
	assert.Equal(int64(len(content)), result.Size)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(int64(len(content)), result.StoredSize)
	assert.True(result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(content, stored)
}

func TestLooseWriteBytesChecksExpectedMetadata(t *testing.T) {
	require := require.New(t)
	content := []byte("actual")
	store := newLooseStoreForTest(t, StagingSameDirectory)

	_, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hashForTest([]byte("other")),
	})
	require.ErrorIs(err, ErrContentMismatch)

	_, err = store.WriteBytes(context.Background(), content, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedSize: int64(len(content) + 1),
		SizeKnown:    true,
	})
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseWriteRejectsHashAndSizeMismatch(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	content := []byte("actual")
	wrongHash := hashForTest([]byte("other"))

	_, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: wrongHash,
	})
	require.ErrorIs(err, ErrContentMismatch)

	actualHash := hashForTest(content)
	_, err = store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: actualHash,
		ExpectedSize: int64(len(content) + 1), SizeKnown: true,
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.NoFileExists(t, store.layout.LoosePath(actualHash))
}

func TestLooseWriteCancellationAndLimitCleanStaging(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := filepath.Join(store.layout.Root(), "tmp")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := store.Write(ctx, bytes.NewReader([]byte("content")), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
	})
	require.ErrorIs(err, context.Canceled)
	assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))

	_, err = store.Write(context.Background(), bytes.NewReader([]byte("too large")), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: 3,
	})
	require.ErrorIs(err, ErrContentMismatch)
	assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))
}

func TestLooseWriteRejectsExistingObjectAboveLimit(t *testing.T) {
	require := require.New(t)
	content := []byte("existing object exceeds limit")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	existing, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: existing.Hash,
		ExpectedSize: existing.Size,
		SizeKnown:    true,
		MaxBytes:     existing.Size - 1,
	})
	require.ErrorIs(err, ErrContentMismatch)
	require.Empty(result)
}

func TestLooseWriteReturnsIdentityAfterCompleteStaging(t *testing.T) {
	require := require.New(t)
	content := []byte("identity survives publication failure")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	hash := hashForTest(content)
	require.NoError(os.WriteFile(filepath.Dir(store.layout.LoosePath(hash)), []byte("not a directory"), 0o600))

	result, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.Error(err)
	assert.Equal(t, hash, result.Hash)
	assert.Equal(t, int64(len(content)), result.Size)
}

func TestLooseWriteRequiresExplicitPolicies(t *testing.T) {
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	_, err := store.Write(context.Background(), bytes.NewReader(nil), WriteOptions{})
	require.ErrorIs(t, err, ErrInvalidPolicy)
}

func TestLooseWriteValidatesCompressionPolicy(t *testing.T) {
	store := newLooseStoreForTest(t, StagingSameDirectory)
	for _, tt := range []struct {
		name        string
		compression LooseCompressionOptions
		wantErr     bool
	}{
		{
			name:        "negative minimum bytes",
			compression: LooseCompressionOptions{MinBytes: -1},
			wantErr:     true,
		},
		{
			name:        "negative minimum savings",
			compression: LooseCompressionOptions{MinSavingsPercent: -1},
			wantErr:     true,
		},
		{
			name:        "minimum savings above 100",
			compression: LooseCompressionOptions{MinSavingsPercent: 101},
			wantErr:     true,
		},
		{
			name:        "minimum savings of 100",
			compression: LooseCompressionOptions{MinSavingsPercent: 100},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.WriteBytes(context.Background(), []byte("content"), WriteOptions{
				Durability:  AtomicPublication,
				Dedup:       VerifyFullHash,
				Compression: tt.compression,
			})

			if tt.wantErr {
				require.ErrorIs(t, err, ErrInvalidPolicy)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestLooseWriteSupportsEmptyAndStoreDirectoryStaging(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	result, err := store.Write(context.Background(), bytes.NewReader(nil), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyTypeAndSize,
	})
	require.NoError(err)
	assert.Equal(hashForTest(nil), result.Hash)
	assert.Zero(result.Size)
	assert.FileExists(result.Path)
	assert.Empty(matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
}

func TestLooseDurableWriteSurfacesExistingFileSyncFailure(t *testing.T) {
	require := require.New(t)
	content := []byte("durable sync failure")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	syncErr := errors.New("injected loose file sync failure")
	originalSync := syncLooseFile
	syncLooseFile = func(*os.File) error { return syncErr }
	t.Cleanup(func() { syncLooseFile = originalSync })

	_, err = store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})
	require.ErrorIs(err, syncErr)
	assert.FileExists(t, created.Path)
}

func TestLooseDurableWriteRetriesRootSyncAfterDirectoryResidue(t *testing.T) {
	require := require.New(t)
	content := []byte("retry parent directory durability")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	syncErr := errors.New("injected root sync failure")
	originalSyncDir := pack.SyncDir
	var rootSyncs int
	pack.SyncDir = func(path string) error {
		if filepath.Clean(path) == filepath.Clean(store.layout.Root()) {
			rootSyncs++
			if rootSyncs == 1 {
				return syncErr
			}
		}
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })
	opts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}

	_, err := store.WriteBytes(context.Background(), content, opts)
	require.ErrorIs(err, syncErr)
	_, err = store.WriteBytes(context.Background(), content, opts)
	require.NoError(err)
	assert.Equal(t, 2, rootSyncs, "existing directory residue must not suppress the parent sync retry")
}

func TestLooseDurableWriteSyncsRootForExistingObject(t *testing.T) {
	require := require.New(t)
	content := []byte("upgrade existing object durability")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	_, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	originalSyncDir := pack.SyncDir
	var synced []string
	pack.SyncDir = func(path string) error {
		synced = append(synced, filepath.Clean(path))
		return originalSyncDir(path)
	}
	t.Cleanup(func() { pack.SyncDir = originalSyncDir })

	_, err = store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	assert.Contains(t, synced, filepath.Clean(store.layout.Root()))
}

func TestLooseWriteDedupVerificationPolicies(t *testing.T) {
	require := require.New(t)
	content := []byte("right")
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	path := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(os.WriteFile(path, []byte("wrong"), 0o600))

	result, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyTypeAndSize,
		ExpectedHash: hash, ExpectedSize: int64(len(content)), SizeKnown: true,
	})
	require.NoError(err)
	assert.False(t, result.Created)
	stored, err := os.ReadFile(path)
	require.NoError(err)
	assert.Equal(t, []byte("wrong"), stored, "structural dedup deliberately does not detect same-size bit rot")

	_, err = store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash,
		ExpectedHash: hash, ExpectedSize: int64(len(content)), SizeKnown: true,
	})
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseVerifyChecksCanonicalObject(t *testing.T) {
	require := require.New(t)
	content := []byte("verify existing object")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, exists, err := store.Verify(created.Hash, created.Size, VerifyFullHash, AtomicPublication)
	require.NoError(err)
	assert.True(t, exists)
	assert.Equal(t, created.Hash, result.Hash)

	missing := hashForTest([]byte("missing"))
	_, exists, err = store.Verify(missing, 0, VerifyFullHash, AtomicPublication)
	require.NoError(err)
	assert.False(t, exists)
	_, _, err = store.Verify("", 0, VerifyFullHash, AtomicPublication)
	require.ErrorIs(err, ErrInvalidHash)
}

func TestLooseDurableVerifyRejectsIdentitySwap(t *testing.T) {
	require := require.New(t)
	content := []byte("durable identity must remain stable")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)
	displaced := created.Path + ".displaced"
	originalSnapshot := snapshotLoosePathIdentity
	var snapshots int
	snapshotLoosePathIdentity = func(path string) (fs.FileInfo, error) {
		if filepath.Clean(path) == filepath.Clean(created.Path) {
			snapshots++
			if snapshots == 2 {
				require.NoError(os.Rename(created.Path, displaced))
				require.NoError(os.WriteFile(created.Path, content, 0o600))
			}
		}
		return originalSnapshot(path)
	}
	t.Cleanup(func() { snapshotLoosePathIdentity = originalSnapshot })

	_, _, err = store.Verify(created.Hash, created.Size, VerifyFullHash, DurablePublication)
	require.ErrorIs(err, errIdentityChanged)
	assert.FileExists(t, created.Path)
	assert.FileExists(t, displaced)
}

func TestLooseWriteFullHashRejectsChangedFileWithRestoredTimestamp(t *testing.T) {
	require := require.New(t)
	content := []byte("right")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash}
	result, err := store.WriteBytes(context.Background(), content, opts)
	require.NoError(err)
	_, err = store.WriteBytes(context.Background(), content, opts)
	require.NoError(err)

	before, err := os.Stat(result.Path)
	require.NoError(err)
	require.NoError(os.WriteFile(result.Path, []byte("wrong"), 0o600))
	require.NoError(os.Chtimes(result.Path, before.ModTime(), before.ModTime()))
	_, err = store.WriteBytes(context.Background(), content, opts)
	require.ErrorIs(err, ErrContentMismatch)
}

func TestLooseWriteMaxIntLimitDoesNotOverflow(t *testing.T) {
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	content := []byte("max-int limit remains bounded by io.Copy's int64 result")
	result, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, MaxBytes: math.MaxInt64,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), result.Size)
}

func TestLooseStoreDefersMissingRootCreationUntilWritePolicyIsKnown(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	root := filepath.Join(t.TempDir(), "missing", "store")
	layout, err := NewLayout(root, LayoutOptions{Staging: StagingStoreDirectory, StagingDir: "tmp"})
	require.NoError(err)
	store, err := NewLooseStore(layout)
	require.NoError(err)
	assert.NoDirExists(root)
	_, err = store.WriteBytes(context.Background(), []byte("durable root"), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyFullHash,
	})
	require.NoError(err)
	assert.DirExists(root)
}

func TestLooseWriteConcurrentDedup(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("concurrent"), 1024)
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: hash}

	const writers = 8
	results := make(chan WriteResult, writers)
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for range writers {
		wg.Go(func() {
			result, err := store.Write(context.Background(), bytes.NewReader(content), opts)
			results <- result
			errs <- err
		})
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		require.NoError(err)
	}
	for result := range results {
		assert.Equal(t, hash, result.Hash)
	}
	stored, err := os.ReadFile(store.layout.LoosePath(hash))
	require.NoError(err)
	assert.Equal(t, content, stored)
}

func TestLooseWriteRejectsSymlinkDestination(t *testing.T) {
	require := require.New(t)
	content := []byte("content")
	hash := hashForTest(content)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	path := store.layout.LoosePath(hash)
	require.NoError(os.MkdirAll(filepath.Dir(path), 0o700))
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(os.WriteFile(target, content, 0o600))
	require.NoError(os.Symlink(target, path))

	_, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability: AtomicPublication, Dedup: VerifyFullHash, ExpectedHash: hash,
	})
	require.Error(err)
	info, statErr := os.Lstat(path)
	require.NoError(statErr)
	assert.NotZero(t, info.Mode()&os.ModeSymlink)
}

func TestRemoveLooseUsesExplicitDurability(t *testing.T) {
	require := require.New(t)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	result, err := store.Write(context.Background(), bytes.NewReader([]byte("remove")), WriteOptions{
		Durability: DurablePublication, Dedup: VerifyFullHash,
	})
	require.NoError(err)
	require.NoError(store.Remove(result.Hash, BestEffortRemoval))
	assert.NoFileExists(t, result.Path)
	require.NoError(store.Remove(result.Hash, DurableRemoval), "missing durable removal is idempotent")
	require.ErrorIs(store.Remove(result.Hash, 0), ErrInvalidPolicy)
}

func BenchmarkLooseWriteBytesDuplicate(b *testing.B) {
	content := bytes.Repeat([]byte("duplicate loose content\n"), 4096)
	store := newLooseStoreForTest(b, StagingSameDirectory)
	opts := WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash}
	_, err := store.WriteBytes(context.Background(), content, opts)
	require.NoError(b, err)
	b.SetBytes(int64(len(content)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_, err := store.WriteBytes(context.Background(), content, opts)
		require.NoError(b, err)
	}
}

func newLooseStoreForTest(t testing.TB, staging StagingMode) *LooseStore {
	t.Helper()
	opts := LayoutOptions{Staging: staging}
	if staging == StagingStoreDirectory {
		opts.StagingDir = "tmp"
	}
	layout, err := NewLayout(t.TempDir(), opts)
	require.NoError(t, err)
	store, err := NewLooseStore(layout)
	require.NoError(t, err)
	return store
}

func hashForTest(content []byte) Hash {
	sum := sha256.Sum256(content)
	hash, err := ParseHash(string(bytesToHex(sum[:])))
	if err != nil {
		panic(err)
	}
	return hash
}

func bytesToHex(data []byte) []byte {
	const digits = "0123456789abcdef"
	encoded := make([]byte, len(data)*2)
	for i, value := range data {
		encoded[i*2] = digits[value>>4]
		encoded[i*2+1] = digits[value&0x0f]
	}
	return encoded
}

func matchingFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern+"*"))
	require.NoError(t, err)
	return matches
}

type boundedChunkReader struct {
	r   io.Reader
	max int
}

func (r *boundedChunkReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		return 0, errors.New("write attempted an unbounded source read")
	}
	return r.r.Read(p)
}
