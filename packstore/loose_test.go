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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

var errInjectedPrimary = errors.New("injected primary failure")

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

func TestLooseWriteCompressionCancellationCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("cancel compressed staging\n"), 4096)
	for _, staging := range []StagingMode{StagingSameDirectory, StagingStoreDirectory} {
		for _, cancelAt := range []string{"source read", "zstd write"} {
			t.Run(cancelAt+"/"+stagingName(staging), func(t *testing.T) {
				require := require.New(t)
				store := newLooseStoreForTest(t, staging)
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				src := io.Reader(&cancelAfterRead{reader: bytes.NewReader(content), cancel: cancel})
				if cancelAt == "zstd write" {
					src = bytes.NewReader(content)
					originalWriter := newLooseZstdWriter
					newLooseZstdWriter = func(dst io.Writer) (io.WriteCloser, error) {
						writer, err := originalWriter(dst)
						if err != nil {
							return nil, err
						}
						return &cancelOnWriteCloser{WriteCloser: writer, cancel: cancel}, nil
					}
					t.Cleanup(func() { newLooseZstdWriter = originalWriter })
				}
				opts := WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest(content),
					Compression: LooseCompressionOptions{
						Enabled: true,
					},
				}

				_, err := store.Write(ctx, src, opts)

				require.ErrorIs(err, context.Canceled)
				assertNoLooseWriteResidue(t, store, hashForTest(content))
			})
		}
	}
}

func TestLooseWriteCompressionFailureCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("zstd write failure\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	writeErr := errors.New("injected zstd write failure")
	originalWriter := newLooseZstdWriter
	newLooseZstdWriter = func(io.Writer) (io.WriteCloser, error) {
		return &errorWriteCloser{err: writeErr}, nil
	}
	t.Cleanup(func() { newLooseZstdWriter = originalWriter })

	_, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, writeErr)
	assertNoLooseWriteResidue(t, store, hashForTest(content))
}

func TestLooseWritePublicationFailureCleansStaging(t *testing.T) {
	content := bytes.Repeat([]byte("publication failure\n"), 4096)
	publishErr := errors.New("injected publication failure")
	originalPublish := publishLooseFile
	publishLooseFile = func(string, string) error { return publishErr }
	t.Cleanup(func() { publishLooseFile = originalPublish })

	for _, staging := range []StagingMode{StagingSameDirectory, StagingStoreDirectory} {
		t.Run(stagingName(staging), func(t *testing.T) {
			store := newLooseStoreForTest(t, staging)
			result, err := store.Write(context.Background(), bytes.NewReader(content), WriteOptions{
				Durability:   AtomicPublication,
				Dedup:        VerifyFullHash,
				ExpectedHash: hashForTest(content),
				Compression: LooseCompressionOptions{
					Enabled: true,
				},
			})

			require.ErrorIs(t, err, publishErr)
			assert.Equal(t, hashForTest(content), result.Hash)
			assertNoLooseWriteResidue(t, store, result.Hash)
		})
	}
}

func TestLooseWriteCompressedDurabilitySyncsSelectedFileAndShard(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("durable compressed content\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	originalFileSync := syncLooseFile
	originalDirSync := pack.SyncDir
	var fileSyncs int
	var syncedDirs []string
	syncLooseFile = func(file *os.File) error {
		fileSyncs++
		return originalFileSync(file)
	}
	pack.SyncDir = func(path string) error {
		syncedDirs = append(syncedDirs, filepath.Clean(path))
		return originalDirSync(path)
	}
	t.Cleanup(func() {
		syncLooseFile = originalFileSync
		pack.SyncDir = originalDirSync
	})

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	require.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(t, 1, fileSyncs, "only the selected staging file is synced")
	assert.Contains(t, syncedDirs, filepath.Clean(filepath.Dir(result.Path)))
	assert.NoFileExists(t, store.layout.LoosePath(result.Hash))
	assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
	assert.FileExists(t, result.Path)
}

func TestLooseWriteDurableStoreStagingSyncsAllUnlinks(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("durable staging unlink\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	originalSync := syncLooseStagingDir
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"), "all staging unlinks precede their durability sync")
		return originalSync(path)
	}
	t.Cleanup(func() { syncLooseStagingDir = originalSync })

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	assert.Equal(1, syncs)
	assert.FileExists(result.Path)
	assert.NoFileExists(store.layout.LoosePath(result.Hash))
}

func TestLooseWriteDurableStoreStagingSyncsCancellationCleanup(t *testing.T) {
	assert := assert.New(t)
	content := bytes.Repeat([]byte("durable cancelled staging\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	originalSync := syncLooseStagingDir
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		return originalSync(path)
	}
	t.Cleanup(func() { syncLooseStagingDir = originalSync })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	_, err := store.Write(ctx, &cancelAfterRead{reader: bytes.NewReader(content), cancel: cancel}, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(1, syncs)
}

func TestLooseWriteDurableStoreStagingSyncsCreationFailureCleanup(t *testing.T) {
	assert := assert.New(t)
	content := []byte("staging creation cleanup")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	stagingDir := store.layout.LooseStagingDir(hashForTest(content))
	chmodErr := errors.New("injected staging chmod failure")
	originalChmod := chmodLooseStagingFile
	originalSync := syncLooseStagingDir
	chmodLooseStagingFile = func(*os.File, fs.FileMode) error { return chmodErr }
	var syncs int
	syncLooseStagingDir = func(path string) error {
		syncs++
		assert.Equal(filepath.Clean(stagingDir), filepath.Clean(path))
		assert.Empty(matchingFiles(t, stagingDir, ".staging-"))
		return originalSync(path)
	}
	t.Cleanup(func() {
		chmodLooseStagingFile = originalChmod
		syncLooseStagingDir = originalSync
	})

	_, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: DurablePublication,
		Dedup:      VerifyFullHash,
	})

	require.ErrorIs(t, err, chmodErr)
	assert.Equal(1, syncs)
}

func TestLooseWriteDurableStagingSyncFailureIsReturned(t *testing.T) {
	content := bytes.Repeat([]byte("staging sync failure\n"), 4096)
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	syncErr := errors.New("injected staging directory sync failure")
	originalSync := syncLooseStagingDir
	syncLooseStagingDir = func(string) error { return syncErr }
	t.Cleanup(func() { syncLooseStagingDir = originalSync })

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability:  DurablePublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, syncErr)
	assert.True(t, result.Created)
	assert.FileExists(t, result.Path)
	assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(result.Hash), ".staging-"))
}

func TestLooseWriteJoinsStagingCloseFailureOnEarlyReturns(t *testing.T) {
	for _, tt := range []struct {
		name        string
		configure   func(*testing.T, context.CancelFunc, error) io.Reader
		opts        func([]byte) WriteOptions
		wantPrimary func(error) bool
	}{
		{
			name: "cancellation",
			configure: func(_ *testing.T, cancel context.CancelFunc, _ error) io.Reader {
				return &cancelAfterRead{reader: bytes.NewReader(bytes.Repeat([]byte("cancel"), 4096)), cancel: cancel}
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, context.Canceled) },
		},
		{
			name: "copy failure",
			configure: func(_ *testing.T, _ context.CancelFunc, primary error) io.Reader {
				return &errorReader{err: primary}
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, errInjectedPrimary) },
		},
		{
			name: "encoder failure",
			configure: func(t *testing.T, _ context.CancelFunc, primary error) io.Reader {
				originalWriter := newLooseZstdWriter
				newLooseZstdWriter = func(io.Writer) (io.WriteCloser, error) {
					return &errorWriteCloser{err: primary}, nil
				}
				t.Cleanup(func() { newLooseZstdWriter = originalWriter })
				return bytes.NewReader(bytes.Repeat([]byte("encoder"), 4096))
			},
			opts: func([]byte) WriteOptions {
				return WriteOptions{Durability: AtomicPublication, Dedup: VerifyFullHash, Compression: LooseCompressionOptions{Enabled: true}}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, errInjectedPrimary) },
		},
		{
			name: "validation failure",
			configure: func(_ *testing.T, _ context.CancelFunc, _ error) io.Reader {
				return bytes.NewReader([]byte("validation content"))
			},
			opts: func(content []byte) WriteOptions {
				return WriteOptions{
					Durability:   AtomicPublication,
					Dedup:        VerifyFullHash,
					ExpectedHash: hashForTest([]byte("different content")),
					Compression:  LooseCompressionOptions{Enabled: true},
				}
			},
			wantPrimary: func(err error) bool { return errors.Is(err, ErrContentMismatch) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cleanupErr := errors.New("injected staging close failure")
			primaryErr := errInjectedPrimary
			store := newLooseStoreForTest(t, StagingStoreDirectory)
			ctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			content := []byte("validation content")
			originalClose := closeLooseStagingFile
			closeLooseStagingFile = func(file *os.File) error {
				return errors.Join(originalClose(file), cleanupErr)
			}
			t.Cleanup(func() { closeLooseStagingFile = originalClose })
			src := tt.configure(t, cancel, primaryErr)

			_, err := store.Write(ctx, src, tt.opts(content))

			require.Error(t, err)
			assert.True(t, tt.wantPrimary(err), "primary failure must remain in the returned error: %v", err)
			require.ErrorIs(t, err, cleanupErr)
		})
	}
}

func TestLooseWriteJoinsStagingRemoveFailure(t *testing.T) {
	primaryErr := errors.New("injected source failure")
	cleanupErr := errors.New("injected staging remove failure")
	store := newLooseStoreForTest(t, StagingStoreDirectory)
	originalRemove := removeLooseStagingFile
	removeLooseStagingFile = func(path string) error {
		return errors.Join(originalRemove(path), cleanupErr)
	}
	t.Cleanup(func() { removeLooseStagingFile = originalRemove })

	_, err := store.Write(context.Background(), &errorReader{err: primaryErr}, WriteOptions{
		Durability:  AtomicPublication,
		Dedup:       VerifyFullHash,
		Compression: LooseCompressionOptions{Enabled: true},
	})

	require.ErrorIs(t, err, primaryErr)
	require.ErrorIs(t, err, cleanupErr)
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

func TestLooseWriteDeduplicatedRawResultReportsPhysicalMetadata(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := []byte("existing raw physical metadata")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})
	require.NoError(err)

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.NoError(err)
	assert.False(result.Created)
	assert.Equal(created.Path, result.Path)
	assert.Equal(LooseEncodingRaw, result.Encoding)
	assert.Equal(result.Size, result.StoredSize)
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

func TestLooseWriteCompressionPolicy(t *testing.T) {
	compressible := bytes.Repeat([]byte("compressible loose content\n"), 256)
	incompressible := deterministicLooseNoise(8 << 10)
	for _, tt := range []struct {
		name        string
		content     []byte
		compression LooseCompressionOptions
		want        LooseEncoding
	}{
		{
			name:        "disabled",
			content:     compressible,
			compression: LooseCompressionOptions{},
			want:        LooseEncodingRaw,
		},
		{
			name:    "below minimum size",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:  true,
				MinBytes: int64(len(compressible)) + 1,
			},
			want: LooseEncodingRaw,
		},
		{
			name:    "incompressible",
			content: incompressible,
			compression: LooseCompressionOptions{
				Enabled: true,
			},
			want: LooseEncodingRaw,
		},
		{
			name:    "compressible",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:           true,
				MinBytes:          int64(len(compressible)),
				MinSavingsPercent: 90,
			},
			want: LooseEncodingZstd,
		},
		{
			name:    "compressible at high threshold",
			content: compressible,
			compression: LooseCompressionOptions{
				Enabled:           true,
				MinSavingsPercent: 99,
			},
			want: LooseEncodingZstd,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for _, write := range []struct {
				name string
				run  func(*LooseStore) (WriteResult, error)
			}{
				{
					name: "stream",
					run: func(store *LooseStore) (WriteResult, error) {
						return store.Write(context.Background(), bytes.NewReader(tt.content), WriteOptions{
							Durability:   AtomicPublication,
							Dedup:        VerifyFullHash,
							ExpectedHash: hashForTest(tt.content),
							Compression:  tt.compression,
						})
					},
				},
				{
					name: "bytes",
					run: func(store *LooseStore) (WriteResult, error) {
						return store.WriteBytes(context.Background(), tt.content, WriteOptions{
							Durability:  AtomicPublication,
							Dedup:       VerifyFullHash,
							Compression: tt.compression,
						})
					},
				},
			} {
				t.Run(write.name, func(t *testing.T) {
					assert := assert.New(t)
					require := require.New(t)
					store := newLooseStoreForTest(t, StagingSameDirectory)

					result, err := write.run(store)

					require.NoError(err)
					assert.Equal(hashForTest(tt.content), result.Hash, "hash describes logical content")
					assert.Equal(int64(len(tt.content)), result.Size, "size describes logical content")
					assert.Equal(tt.want, result.Encoding)
					assert.True(result.Created)
					assert.FileExists(result.Path)
					if tt.want == LooseEncodingZstd {
						assert.Equal(store.layout.CompressedLoosePath(result.Hash), result.Path)
						assert.Less(result.StoredSize, result.Size)
						assert.NoFileExists(store.layout.LoosePath(result.Hash))
					} else {
						assert.Equal(store.layout.LoosePath(result.Hash), result.Path)
						assert.Equal(result.Size, result.StoredSize)
						assert.NoFileExists(store.layout.CompressedLoosePath(result.Hash))
					}
					assert.Empty(matchingFiles(t, filepath.Dir(result.Path), ".staging-"))
				})
			}
		})
	}
}

func TestLooseWriteCompressionExactSavingsBoundaryIncludesHeader(t *testing.T) {
	content := bytes.Repeat([]byte("x"), 100)
	originalWriter := newLooseZstdWriter
	newLooseZstdWriter = func(dst io.Writer) (io.WriteCloser, error) {
		return &fixedSizeEncoder{dst: dst, payloadBytes: 64}, nil
	}
	t.Cleanup(func() { newLooseZstdWriter = originalWriter })

	for _, tt := range []struct {
		name       string
		minSavings int
		want       LooseEncoding
		storedSize int64
	}{
		{
			name:       "exact twenty percent is inclusive",
			minSavings: 20,
			want:       LooseEncodingZstd,
			storedSize: 80,
		},
		{
			name:       "one percent above exact boundary is raw",
			minSavings: 21,
			want:       LooseEncodingRaw,
			storedSize: 100,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := newLooseStoreForTest(t, StagingSameDirectory)

			result, err := store.WriteBytes(context.Background(), content, WriteOptions{
				Durability: AtomicPublication,
				Dedup:      VerifyFullHash,
				Compression: LooseCompressionOptions{
					Enabled:           true,
					MinSavingsPercent: tt.minSavings,
				},
			})

			require.NoError(t, err)
			assert.Equal(t, tt.want, result.Encoding)
			assert.Equal(t, tt.storedSize, result.StoredSize)
			if tt.want == LooseEncodingZstd {
				assert.FileExists(t, store.layout.CompressedLoosePath(result.Hash))
				assert.NoFileExists(t, store.layout.LoosePath(result.Hash))
			} else {
				assert.FileExists(t, store.layout.LoosePath(result.Hash))
				assert.NoFileExists(t, store.layout.CompressedLoosePath(result.Hash))
			}
		})
	}
}

func TestLooseWriteCompressionStreamsSourceWithPooledBuffer(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("bounded compressed source\n"), 64*1024)
	store := newLooseStoreForTest(t, StagingSameDirectory)

	result, err := store.Write(context.Background(), &boundedChunkReader{
		r:   bytes.NewReader(content),
		max: 32 << 10,
	}, WriteOptions{
		Durability:   AtomicPublication,
		Dedup:        VerifyFullHash,
		ExpectedHash: hashForTest(content),
		Compression:  LooseCompressionOptions{Enabled: true},
	})

	require.NoError(err)
	assert.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(int64(len(content)), result.Size)
	assert.Less(result.StoredSize, result.Size)
	assert.FileExists(result.Path)
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

func TestLooseWriteDedupPrefersCompressedRepresentation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("preferred compressed representation\n"), 256)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled:           true,
			MinSavingsPercent: 50,
		},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)
	rawPath := store.layout.LoosePath(created.Hash)
	require.NoError(os.WriteFile(rawPath, content, 0o600))

	result, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.NoError(err)
	assert.False(result.Created)
	assert.Equal(LooseEncodingZstd, result.Encoding)
	assert.Equal(created.Path, result.Path)
	assert.Equal(created.StoredSize, result.StoredSize)
	assert.FileExists(rawPath, "dedup leaves dual-copy reconciliation to maintenance")
	assert.FileExists(created.Path)
}

func TestLooseWriteDedupRejectsCorruptPreferredRepresentation(t *testing.T) {
	require := require.New(t)
	content := bytes.Repeat([]byte("do not fall back from corrupt preferred content\n"), 256)
	store := newLooseStoreForTest(t, StagingSameDirectory)
	created, err := store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
		Compression: LooseCompressionOptions{
			Enabled: true,
		},
	})
	require.NoError(err)
	require.Equal(LooseEncodingZstd, created.Encoding)
	rawPath := store.layout.LoosePath(created.Hash)
	require.NoError(os.WriteFile(rawPath, content, 0o600))
	corrupt := []byte("corrupt preferred copy")
	require.NoError(os.WriteFile(created.Path, corrupt, 0o600))

	_, err = store.WriteBytes(context.Background(), content, WriteOptions{
		Durability: AtomicPublication,
		Dedup:      VerifyFullHash,
	})

	require.ErrorIs(err, ErrContentMismatch)
	assert.Equal(t, corrupt, mustReadFile(t, created.Path), "ordinary writes must not replace a corrupt preferred copy")
	assert.Equal(t, content, mustReadFile(t, rawPath), "a valid alternate copy must not mask preferred corruption")
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

func TestLooseWriteConcurrentRawAndCompressedPublishOneRepresentation(t *testing.T) {
	assert := assert.New(t)
	require := require.New(t)
	content := bytes.Repeat([]byte("cross-representation publication\n"), 4096)
	hash := hashForTest(content)
	layout, err := NewLayout(t.TempDir(), LayoutOptions{Staging: StagingSameDirectory})
	require.NoError(err)
	rawStore, err := NewLooseStore(layout)
	require.NoError(err)
	compressedStore, err := NewLooseStore(layout)
	require.NoError(err)
	arrived := make(chan LooseEncoding, 2)
	release := make(chan struct{})
	originalBeforePublish := beforeLoosePublish
	beforeLoosePublish = func(gotHash Hash, encoding LooseEncoding) {
		if gotHash == hash {
			arrived <- encoding
			<-release
		}
	}
	t.Cleanup(func() { beforeLoosePublish = originalBeforePublish })

	type writeOutcome struct {
		result WriteResult
		err    error
	}
	outcomes := make(chan writeOutcome, 2)
	var writers sync.WaitGroup
	writers.Go(func() {
		result, err := rawStore.WriteBytes(context.Background(), content, WriteOptions{
			Durability: AtomicPublication,
			Dedup:      VerifyFullHash,
		})
		outcomes <- writeOutcome{result: result, err: err}
	})
	writers.Go(func() {
		result, err := compressedStore.WriteBytes(context.Background(), content, WriteOptions{
			Durability:  AtomicPublication,
			Dedup:       VerifyFullHash,
			Compression: LooseCompressionOptions{Enabled: true},
		})
		outcomes <- writeOutcome{result: result, err: err}
	})

	firstArrival := receiveLooseEncoding(t, arrived)
	secondArrival := receiveLooseEncoding(t, arrived)
	assert.ElementsMatch([]LooseEncoding{LooseEncodingRaw, LooseEncodingZstd}, []LooseEncoding{firstArrival, secondArrival})
	close(release)
	writers.Wait()
	close(outcomes)

	var results []WriteResult
	for outcome := range outcomes {
		require.NoError(outcome.err)
		results = append(results, outcome.result)
	}
	require.Len(results, 2)
	assert.Equal(results[0].Path, results[1].Path)
	assert.Equal(results[0].Encoding, results[1].Encoding)
	created := 0
	for _, result := range results {
		if result.Created {
			created++
		}
	}
	assert.Equal(1, created)
	canonicalFiles := 0
	for _, path := range []string{layout.LoosePath(hash), layout.CompressedLoosePath(hash)} {
		if _, err := os.Stat(path); err == nil {
			canonicalFiles++
		} else {
			require.ErrorIs(err, fs.ErrNotExist)
		}
	}
	assert.Equal(1, canonicalFiles)
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

func deterministicLooseNoise(size int) []byte {
	content := make([]byte, size)
	state := uint64(0x9e3779b97f4a7c15)
	for i := range content {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		content[i] = byte(state)
	}
	return content
}

func matchingFiles(t *testing.T, dir, pattern string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, pattern+"*"))
	require.NoError(t, err)
	return matches
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	return content
}

func assertNoLooseWriteResidue(t *testing.T, store *LooseStore, hash Hash) {
	t.Helper()
	assert.NoFileExists(t, store.layout.LoosePath(hash))
	assert.NoFileExists(t, store.layout.CompressedLoosePath(hash))
	assert.Empty(t, matchingFiles(t, store.layout.LooseStagingDir(hash), ".staging-"))
}

func stagingName(staging StagingMode) string {
	if staging == StagingSameDirectory {
		return "same directory"
	}
	return "store directory"
}

func receiveLooseEncoding(t *testing.T, values <-chan LooseEncoding) LooseEncoding {
	t.Helper()
	select {
	case value := <-values:
		return value
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for loose writer to reach publication barrier")
		return 0
	}
}

type cancelAfterRead struct {
	reader io.Reader
	cancel context.CancelFunc
	done   bool
}

func (r *cancelAfterRead) Read(p []byte) (int, error) {
	if len(p) > 1024 {
		p = p[:1024]
	}
	n, err := r.reader.Read(p)
	if n > 0 && !r.done {
		r.done = true
		r.cancel()
	}
	return n, err
}

type cancelOnWriteCloser struct {
	io.WriteCloser
	cancel context.CancelFunc
	done   bool
}

func (w *cancelOnWriteCloser) Write(p []byte) (int, error) {
	n, err := w.WriteCloser.Write(p)
	if !w.done {
		w.done = true
		w.cancel()
	}
	return n, err
}

type errorWriteCloser struct {
	err error
}

func (w *errorWriteCloser) Write([]byte) (int, error) { return 0, w.err }
func (w *errorWriteCloser) Close() error              { return nil }

type errorReader struct {
	err error
}

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }

type fixedSizeEncoder struct {
	dst          io.Writer
	payloadBytes int
}

func (w *fixedSizeEncoder) Write(p []byte) (int, error) { return len(p), nil }

func (w *fixedSizeEncoder) Close() error {
	_, err := w.dst.Write(make([]byte, w.payloadBytes))
	return err
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
