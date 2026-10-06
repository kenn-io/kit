package packstore

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

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
				assert.Equal(t, 1, rootSyncs, "one parent sync covers both pre-existing child directories")
				stored, err := os.ReadFile(result.Path)
				require.NoError(err)
				assert.Equal(t, content, stored)
				assert.Empty(t, matchingFiles(t, layout.LooseStagingDir(hash), ".staging-"))
			})
		}
	}
}

func TestLooseDurableWriteRetriesExistingDirectoryParentSync(t *testing.T) {
	content := []byte("retry parent before publishing")
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
