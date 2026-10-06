package packstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

func TestLooseDurableWriteSyncsConcurrentDirectoryCreation(t *testing.T) {
	require := require.New(t)
	content := []byte("concurrent shard creation")
	store := newLooseStoreForTest(t, StagingSameDirectory)
	shard := filepath.Dir(store.layout.LoosePath(hashForTest(content)))
	opts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}
	creatorWaiting := make(chan struct{})
	resumeCreator := make(chan struct{})
	creatorDone := make(chan struct{})
	var creatorErr error
	var releaseOnce sync.Once
	releaseCreator := func() { releaseOnce.Do(func() { close(resumeCreator) }) }
	var startedCreator, blockedSync, rootSynced atomic.Bool
	originalLstat := lstatLooseDirectory
	originalSync := pack.SyncDir
	originalPublish := publishLooseFile
	t.Cleanup(func() {
		lstatLooseDirectory = originalLstat
		pack.SyncDir = originalSync
		publishLooseFile = originalPublish
	})
	t.Cleanup(func() {
		releaseCreator()
		if startedCreator.Load() {
			<-creatorDone
		}
	})
	lstatLooseDirectory = func(path string) (os.FileInfo, error) {
		info, err := originalLstat(path)
		if path == shard && os.IsNotExist(err) && startedCreator.CompareAndSwap(false, true) {
			// Let a second real writer create the missing shard, but pause
			// its parent sync before returning our original missing result.
			go func() {
				_, creatorErr = store.WriteBytes(t.Context(), content, opts)
				close(creatorDone)
			}()
			select {
			case <-creatorWaiting:
			case <-creatorDone:
				return nil, errors.Join(creatorErr, errors.New("concurrent creator returned before parent sync"))
			}
		}
		return info, err
	}
	pack.SyncDir = func(path string) error {
		if path == store.layout.Root() && blockedSync.CompareAndSwap(false, true) {
			close(creatorWaiting)
			<-resumeCreator
		}
		err := originalSync(path)
		if path == store.layout.Root() && err == nil {
			rootSynced.Store(true)
		}
		return err
	}
	publishLooseFile = func(src, dst string) error {
		assert.True(t, rootSynced.Load(), "publication must not depend on the other writer's pending parent sync")
		return originalPublish(src, dst)
	}

	result, err := store.WriteBytes(t.Context(), content, opts)
	require.True(startedCreator.Load(), "the concurrent directory creation must be exercised")
	releaseCreator()
	<-creatorDone
	require.NoError(creatorErr)
	require.NoError(err)
	assert.True(t, result.Created)
	stored, err := os.ReadFile(result.Path)
	require.NoError(err)
	assert.Equal(t, content, stored)
}

func TestLooseDurableWriteSharesStagingAndShardParentSync(t *testing.T) {
	content := []byte("shared parent durability")
	hash := hashForTest(content)
	for _, stagingDir := range []string{"tmp", hash.String()[:2]} {
		for _, operation := range []string{"write", "duplicate", "repair"} {
			t.Run(stagingDir+"/"+operation, func(t *testing.T) {
				require := require.New(t)
				layout, err := NewLayout(t.TempDir(), LayoutOptions{
					Staging: StagingStoreDirectory, StagingDir: stagingDir,
				})
				require.NoError(err)
				store, err := NewLooseStore(layout)
				require.NoError(err)
				// These entries may be residue from an interrupted write;
				// existence alone does not establish their durability.
				require.NoError(os.MkdirAll(layout.LooseStagingDir(hash), 0o700))
				require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
				if operation != "write" {
					stored := content
					if operation == "repair" {
						stored = []byte("damaged")
					}
					require.NoError(os.WriteFile(layout.LoosePath(hash), stored, 0o600))
				}
				originalSync := pack.SyncDir
				originalPublish := publishLooseFile
				originalRepair := publishLooseRepairFile
				var rootSyncs int
				pack.SyncDir = func(path string) error {
					if filepath.Clean(path) == layout.Root() {
						rootSyncs++
					}
					return originalSync(path)
				}
				publishLooseFile = func(src, dst string) error {
					assert.Positive(t, rootSyncs, "parent must be durable before publication")
					return originalPublish(src, dst)
				}
				publishLooseRepairFile = func(src, dst string, identity os.FileInfo) (looseRepairPublishResult, error) {
					assert.Positive(t, rootSyncs, "parent must be durable before repair recovery can need staging")
					return originalRepair(src, dst, identity)
				}
				t.Cleanup(func() {
					pack.SyncDir = originalSync
					publishLooseFile = originalPublish
					publishLooseRepairFile = originalRepair
				})

				var result WriteResult
				if operation == "repair" {
					result, err = store.Repair(t.Context(), bytes.NewReader(content), LooseIdentity{
						Hash: hash, Size: int64(len(content)),
					}, RepairOptions{Durability: DurablePublication})
				} else {
					// No expected hash: duplicates must go through staging too.
					result, err = store.Write(t.Context(), bytes.NewReader(content), WriteOptions{
						Durability: DurablePublication, Dedup: VerifyFullHash,
					})
				}
				require.NoError(err)
				assert.Equal(t, operation != "duplicate", result.Created)
				if operation == "duplicate" {
					assert.LessOrEqual(t, rootSyncs, 1, "staging must not add another parent sync to dedup verification")
				} else {
					assert.Equal(t, 1, rootSyncs, "one parent sync covers both pre-existing child directories")
				}
				stored, err := os.ReadFile(result.Path)
				require.NoError(err)
				assert.Equal(t, content, stored)
				assert.Empty(t, matchingFiles(t, layout.LooseStagingDir(hash), ".staging-"))
			})
		}
	}
}

func TestLooseDurableWriteRequiresExistingDirectoryParentSync(t *testing.T) {
	content := []byte("sync parent before publishing")
	hash := hashForTest(content)
	for _, opts := range []LayoutOptions{
		{Staging: StagingSameDirectory},
		{Staging: StagingStoreDirectory, StagingDir: "tmp"},
		{Staging: StagingStoreDirectory, StagingDir: hash.String()[:2]},
		{Staging: StagingStoreDirectory, StagingDir: "."},
	} {
		t.Run(stagingName(opts.Staging)+"/"+opts.StagingDir, func(t *testing.T) {
			require := require.New(t)
			layout, err := NewLayout(t.TempDir(), opts)
			require.NoError(err)
			store, err := NewLooseStore(layout)
			require.NoError(err)
			stagingDir := layout.LooseStagingDir(hash)
			require.NoError(os.MkdirAll(stagingDir, 0o700))
			require.NoError(os.MkdirAll(filepath.Dir(layout.LoosePath(hash)), 0o700))
			parent := filepath.Dir(stagingDir)
			originalSync := pack.SyncDir
			syncErr := errors.New("injected parent sync failure")
			var parentSyncs int
			pack.SyncDir = func(path string) error {
				if filepath.Clean(path) == parent {
					parentSyncs++
					if parentSyncs == 1 {
						return syncErr
					}
				}
				return originalSync(path)
			}
			t.Cleanup(func() { pack.SyncDir = originalSync })
			writeOpts := WriteOptions{Durability: DurablePublication, Dedup: VerifyFullHash}

			result, err := store.WriteBytes(t.Context(), content, writeOpts)
			require.ErrorIs(err, syncErr)
			assert.False(t, result.Created)
			assert.NoFileExists(t, layout.LoosePath(hash))
			assert.Empty(t, matchingFiles(t, stagingDir, ".staging-"))

			result, err = store.WriteBytes(t.Context(), content, writeOpts)
			require.NoError(err)
			assert.True(t, result.Created)
			assert.Equal(t, 2, parentSyncs, "retry must establish parent durability again")
			stored, err := os.ReadFile(result.Path)
			require.NoError(err)
			assert.Equal(t, content, stored)
		})
	}
}
