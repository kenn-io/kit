package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/packstore"
)

func TestRestoreLooseCompression(t *testing.T) {
	repo := initTestRepo(t)
	app := packedExtensionApp{App: newTestApp()}
	dbPath, contentDir, dataDir, writer := seedBackupFixture(t)
	refs := []ContentRef{
		writeLargeAttachment(t, contentDir, 64<<20+1, true),
		writeLooseAttachment(t, contentDir, bytes.Repeat([]byte("compressible content\n"), 4096)),
		writeLargeAttachment(t, contentDir, 64<<20+1, false),
	}
	for _, ref := range refs {
		_, err := writer.ExecContext(t.Context(),
			`INSERT INTO blobs (content_hash, storage_path, size) VALUES (?, ?, ?)`,
			ref.Hash, ref.StoragePath, ref.Size)
		require.NoError(t, err)
	}
	manifest, err := Create(t.Context(), repo, app, createOpts(dbPath, contentDir, dataDir, t.TempDir()))
	require.NoError(t, err)
	require.NotEmpty(t, manifest.Attachments.Recipes)
	for _, mode := range []string{"loose", "packed", "publication failure"} {
		t.Run(mode, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "restored")
			var receipts []packstore.WriteResult
			publicationErr := errors.New("reject staged catalog")
			opts := RestoreOptions{
				TargetDir: target,
				LooseCompression: packstore.LooseCompressionOptions{
					Enabled: true, MinBytes: 4096, MinSavingsPercent: 10,
				},
				BeforePublication: func(_ context.Context, staged RestorePublicationTarget) error {
					assert.NoFileExists(t, filepath.Join(target, app.DBFileName()))
					receipts = staged.LooseContent
					if mode == "publication failure" {
						return publicationErr
					}
					return nil
				},
			}
			if mode == "packed" {
				opts.PackedContent = testPackedTarget{
					limits: packstore.DefaultLimits(),
					open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
						return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
							return nil
						}), nil
					},
				}
			}
			_, err := Restore(t.Context(), repo, app, opts)
			if mode == "publication failure" {
				require.ErrorIs(t, err, publicationErr)
				assert.NoFileExists(t, filepath.Join(target, app.DBFileName()))
			} else {
				require.NoError(t, err)
			}
			if mode == "packed" {
				require.Len(t, receipts, 2)
			} else {
				require.Len(t, receipts, int(manifest.Attachments.Blobs))
			}
			layout, err := packstore.NewLayout(filepath.Join(target, "content"), packstore.LayoutOptions{Staging: packstore.StagingSameDirectory})
			require.NoError(t, err)
			loose, err := packstore.NewLooseStore(layout)
			require.NoError(t, err)
			for _, receipt := range receipts {
				verified, exists, err := loose.Verify(receipt.Hash, receipt.Size, packstore.VerifyFullHash, packstore.AtomicPublication)
				require.NoError(t, err)
				require.True(t, exists)
				assert.Equal(t, receipt.Encoding, verified.Encoding)
				assert.Equal(t, receipt.StoredSize, verified.StoredSize)
				if receipt.Hash.String() == refs[0].Hash || receipt.Hash.String() == refs[1].Hash {
					assert.Equal(t, packstore.LooseEncodingZstd, receipt.Encoding)
					assert.Less(t, receipt.StoredSize, receipt.Size/10)
					assert.NoFileExists(t, layout.LoosePath(receipt.Hash))
				} else {
					assert.Equal(t, packstore.LooseEncodingRaw, receipt.Encoding)
				}
			}
		})
	}
}

func TestRestoreLooseCompressionRequiresCatalogAndCanonicalPaths(t *testing.T) {
	repo, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restored")
	opts := RestoreOptions{
		TargetDir: target, LooseCompression: packstore.LooseCompressionOptions{Enabled: true},
	}
	_, err := Restore(t.Context(), repo, app, opts)
	require.ErrorContains(t, err, "requires BeforePublication")
	assert.NoDirExists(t, target)
	opts.BeforePublication = func(context.Context, RestorePublicationTarget) error { return nil }
	_, err = Restore(t.Context(), repo, badContentPathApp{App: app, path: "custom.bin"}, opts)
	require.ErrorContains(t, err, "requires one canonical restore path")
	assert.NoFileExists(t, filepath.Join(target, app.DBFileName()))
}
