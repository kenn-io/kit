package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

type packedExtensionApp struct{ App }

func (a packedExtensionApp) PackFileExtension() string { return packstore.PackExt }

type restoreCatalogFunc func(context.Context, []packstore.PackRecord, []packstore.Adoption) error

func (f restoreCatalogFunc) ReplaceRestoredPacks(
	ctx context.Context, records []packstore.PackRecord, adoptions []packstore.Adoption,
) error {
	return f(ctx, records, adoptions)
}

type testPackedTarget struct {
	limits packstore.Limits
	open   func(context.Context, *sql.DB) (packstore.RestoreCatalog, error)
}

func (t testPackedTarget) Limits() packstore.Limits { return t.limits }

func (t testPackedTarget) OpenRestoreCatalog(ctx context.Context, db *sql.DB) (packstore.RestoreCatalog, error) {
	return t.open(ctx, db)
}

func createPackedRestoreFixture(t *testing.T) (*Repo, App, *Manifest, string) {
	t.Helper()
	ctx := context.Background()
	r := initTestRepo(t)
	app := packedExtensionApp{App: newTestApp()}
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, app, createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(t, err)
	return r, app, m, attachmentsDir
}

func TestRestoreWithoutPackedTargetRemainsFullyLoose(t *testing.T) {
	r, app, m, sourceContent := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")

	res, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	assert.Zero(t, res.PackedAttachmentBlobs)
	assert.Equal(t, m.Attachments.Blobs, res.LooseAttachmentBlobs)
	assert.Empty(t, res.PackFallbacks)
	assert.Zero(t, res.AttachmentPacks)
	_, err = os.Stat(filepath.Join(target, "content", "packs"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, snapshotDirHashes(t, sourceContent), snapshotDirHashes(t, filepath.Join(target, "content")))
}

func TestRestorePackedTargetPublishesThenCommitsBeforeProof(t *testing.T) {
	r, app, m, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	committed := false
	packed := testPackedTarget{limits: packstore.DefaultLimits()}
	packed.open = func(ctx context.Context, db *sql.DB) (packstore.RestoreCatalog, error) {
		var notes int
		require.NoError(t, db.QueryRowContext(ctx, "SELECT COUNT(*) FROM notes").Scan(&notes))
		require.Positive(t, notes)
		return restoreCatalogFunc(func(commitCtx context.Context, records []packstore.PackRecord, adoptions []packstore.Adoption) error {
			require.NotEmpty(t, records)
			require.Len(t, adoptions, int(m.Attachments.Blobs))
			for _, record := range records {
				_, err := os.Stat(filepath.Join(target, "content", "packs", record.PackID[:2], record.PackID+packstore.PackExt))
				require.NoError(t, err, "pack must be durable before authority is granted")
			}
			tx, err := db.BeginTx(commitCtx, nil)
			require.NoError(t, err)
			_, err = tx.ExecContext(commitCtx, "CREATE TABLE restored_pack_authority (packed_blobs INTEGER NOT NULL)")
			require.NoError(t, err)
			_, err = tx.ExecContext(commitCtx, "INSERT INTO restored_pack_authority VALUES (?)", len(adoptions))
			require.NoError(t, err)
			require.NoError(t, tx.Commit())
			committed = true
			return nil
		}), nil
	}
	proofApp := proofObservingApp{App: app, beforeStats: func() { require.True(t, committed) }}
	sawAttachmentStart := false

	res, err := Restore(context.Background(), r, proofApp, RestoreOptions{
		TargetDir: target, PackedContent: packed,
		Progress: func(event ProgressEvent) {
			if event.Stage == ProgressStageAttachments && event.Done == 0 && !sawAttachmentStart {
				sawAttachmentStart = true
				_, statErr := os.Stat(filepath.Join(target, "content", "packs"))
				require.ErrorIs(t, statErr, os.ErrNotExist,
					"attachment progress must begin before pack preparation publishes files")
			}
		},
	})
	require.NoError(t, err)
	assert.True(t, committed)
	assert.True(t, sawAttachmentStart)
	assert.Equal(t, m.Attachments.Blobs, res.PackedAttachmentBlobs)
	assert.Zero(t, res.LooseAttachmentBlobs)
	assert.Positive(t, res.AttachmentPacks)
	assert.Equal(t, res.AttachmentBlobs, res.PackedAttachmentBlobs+res.LooseAttachmentBlobs)
	published, err := sql.Open("sqlite3", res.DBPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = published.Close() })
	var publishedPacked int64
	require.NoError(t, published.QueryRow("SELECT packed_blobs FROM restored_pack_authority").Scan(&publishedPacked))
	assert.Equal(t, m.Attachments.Blobs, publishedPacked)
}

type proofObservingApp struct {
	App
	beforeStats func()
	badStats    bool
}

type contentPathOverrideApp struct {
	App
	override func(map[string][]string)
}

func (a contentPathOverrideApp) RestoredContentPaths(
	ctx context.Context, db *sql.DB,
) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	a.override(paths)
	return paths, nil
}

func (a proofObservingApp) RestoredStats(ctx context.Context, db *sql.DB) (json.RawMessage, error) {
	if a.beforeStats != nil {
		a.beforeStats()
	}
	if a.badStats {
		return json.RawMessage(`{"proof":"failed"}`), nil
	}
	return a.App.RestoredStats(ctx, db)
}

func TestRestorePackedTargetFallsBackDeclinedEntriesLoose(t *testing.T) {
	r, app, m, sourceContent := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 25
	var adopted []packstore.Adoption
	packed := testPackedTarget{limits: limits, open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(_ context.Context, _ []packstore.PackRecord, got []packstore.Adoption) error {
			adopted = append(adopted, got...)
			return nil
		}), nil
	}}

	res, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, PackedContent: packed})
	require.NoError(t, err)
	assert.Positive(t, res.PackedAttachmentBlobs)
	assert.Positive(t, res.LooseAttachmentBlobs)
	assert.Equal(t, m.Attachments.Blobs, res.PackedAttachmentBlobs+res.LooseAttachmentBlobs)
	assert.Len(t, adopted, int(res.PackedAttachmentBlobs))
	assert.NotEmpty(t, res.PackFallbacks)
	sourceHashes := snapshotDirHashes(t, sourceContent)
	wantLoose := map[string][32]byte{}
	for _, fallback := range res.PackFallbacks {
		if fallback.Hash != "" {
			rel := filepath.Join(fallback.Hash.String()[:2], fallback.Hash.String())
			wantLoose[rel] = sourceHashes[rel]
		}
	}
	assert.Equal(t, wantLoose, looseRestoredHashes(t, target, res.PackFallbacks))
}

func looseRestoredHashes(t *testing.T, target string, fallbacks []packstore.ImportFallback) map[string][32]byte {
	t.Helper()
	result := map[string][32]byte{}
	for _, fallback := range fallbacks {
		if fallback.Hash == "" {
			continue
		}
		rel := filepath.Join(fallback.Hash.String()[:2], fallback.Hash.String())
		result[rel] = fileSHA256(t, filepath.Join(target, "content", rel))
	}
	return result
}

func TestRestorePackedTargetCatalogFailureDoesNotPublishDatabase(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
			return errors.New("catalog rejected restore")
		}), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.ErrorContains(t, err, "catalog rejected restore")
	checkIntact()
}

func TestRestorePackedTargetProofFailureKeepsVisibleDatabase(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	committed := false
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
			committed = true
			return nil
		}), nil
	}}

	_, err := Restore(context.Background(), r, proofObservingApp{App: app, badStats: true}, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.ErrorContains(t, err, "do not match manifest stats")
	assert.True(t, committed)
	checkIntact()
}

func TestRestorePackedTargetOverwriteKeepsOldDatabaseUntilPublish(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target, _ := seedLiveOverwriteTarget(t)
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
			got, err := os.ReadFile(filepath.Join(target, "app.db"))
			require.NoError(t, err)
			require.Equal(t, []byte("live database bytes"), got)
			return nil
		}), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(target, "app.db"))
	require.NoError(t, err)
	assert.NotEqual(t, []byte("live database bytes"), got)
}

func TestRestorePackedTargetCorruptSelectedSourceKeepsVisibleDatabase(t *testing.T) {
	r, app, m, _ := createPackedRestoreFixture(t)
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, packstore.PackExt)
	require.NoError(t, err)
	id, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(t, err)
	ie := known[id]
	sourcePack := r.packPath(ie.PackID, packstore.PackExt)
	data, err := os.ReadFile(sourcePack)
	require.NoError(t, err)
	data[ie.Offset+ie.StoredLen/2] ^= 1
	require.NoError(t, os.WriteFile(sourcePack, data, 0o600))
	target, checkIntact := seedLiveOverwriteTarget(t)
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err = Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.Error(t, err)
	checkIntact()
}

func TestRestorePackedTargetIncompatibleExtensionRestoresFullyLooseAndClearsAuthority(t *testing.T) {
	ctx := context.Background()
	r := initTestRepo(t)
	app := newTestApp()
	dbPath, sourceContent, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, app, createOpts(dbPath, sourceContent, dataDir, t.TempDir()))
	require.NoError(t, err)
	target := filepath.Join(t.TempDir(), "restore")
	commits := 0
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(_ context.Context, records []packstore.PackRecord, adoptions []packstore.Adoption) error {
			commits++
			assert.Empty(t, records)
			assert.Empty(t, adoptions)
			return nil
		}), nil
	}}

	res, err := Restore(ctx, r, app, RestoreOptions{TargetDir: target, PackedContent: packed})
	require.NoError(t, err)
	assert.Equal(t, 1, commits)
	assert.Zero(t, res.PackedAttachmentBlobs)
	assert.Equal(t, m.Attachments.Blobs, res.LooseAttachmentBlobs)
	assert.Equal(t, m.Attachments.Blobs, res.AttachmentBlobs)
	assert.Equal(t, m.Attachments.BlobBytes, res.AttachmentBytes)
	assert.NotEmpty(t, res.PackFallbacks)
	for _, fallback := range res.PackFallbacks {
		assert.Equal(t, packstore.FallbackPackEncoding, fallback.Reason)
	}
	_, err = os.Stat(filepath.Join(target, "content", "packs"))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Equal(t, snapshotDirHashes(t, sourceContent), snapshotDirHashes(t, filepath.Join(target, "content")))
}

func TestRestorePackedTargetUnsupportedEncodingStillRequiresLooseVerification(t *testing.T) {
	r, app, m, sourceContent := createPackedRestoreFixture(t)
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, packstore.PackExt)
	require.NoError(t, err)
	attachmentIDs := make(map[pack.BlobID]struct{}, len(refs))
	appender := NewPackAppender(r, map[pack.BlobID]IndexEntry{}, pack.DefaultZstdLevel, nil, packstore.PackExt)
	for _, ref := range refs {
		id, parseErr := pack.ParseBlobID(ref.Hash)
		require.NoError(t, parseErr)
		attachmentIDs[id] = struct{}{}
		raw, readErr := os.ReadFile(filepath.Join(sourceContent, ref.Hash[:2], ref.Hash))
		require.NoError(t, readErr)
		_, _, addErr := appender.Add(raw)
		require.NoError(t, addErr)
	}
	packIDs, replacementEntries, err := appender.Finish()
	require.NoError(t, err)
	require.Len(t, packIDs, 1)
	combined := append([]IndexEntry(nil), replacementEntries...)
	for id, entry := range known {
		if _, replaced := attachmentIDs[id]; !replaced {
			combined = append(combined, entry)
		}
	}
	indexFiles, err := os.ReadDir(r.Path("indexes"))
	require.NoError(t, err)
	for _, file := range indexFiles {
		require.NoError(t, os.Remove(r.Path("indexes", file.Name())))
	}
	_, err = r.WriteIndex(combined)
	require.NoError(t, err)
	sourcePack := r.packPath(packIDs[0], packstore.PackExt)
	data, err := os.ReadFile(sourcePack)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(data), 5)
	data[4] = 0x7f // recognizable pack magic with an unsupported version
	require.NoError(t, os.WriteFile(sourcePack, data, 0o600))
	target, checkIntact := seedLiveOverwriteTarget(t)
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return nil, errors.New("catalog must not open when loose verification fails")
	}}

	_, err = Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.ErrorContains(t, err, "opening pack")
	assert.NotContains(t, err.Error(), "preparing packed attachment restore")
	checkIntact()
}

func TestRestorePackedTargetZeroBlobLimitRestoresFullyLoose(t *testing.T) {
	r, app, m, sourceContent := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 0
	committed := false
	packed := testPackedTarget{limits: limits, open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(_ context.Context, records []packstore.PackRecord, adoptions []packstore.Adoption) error {
			assert.Empty(t, records)
			assert.Empty(t, adoptions)
			committed = true
			return nil
		}), nil
	}}

	res, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, PackedContent: packed})
	require.NoError(t, err)
	assert.True(t, committed)
	assert.Zero(t, res.PackedAttachmentBlobs)
	assert.Equal(t, m.Attachments.Blobs, res.LooseAttachmentBlobs)
	assert.Equal(t, snapshotDirHashes(t, sourceContent), snapshotDirHashes(t, filepath.Join(target, "content")))
	_, err = os.Stat(filepath.Join(target, "content", "packs"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRestorePackedTargetRejectsNegativeBlobLimitBeforePublishingContent(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	limits := packstore.DefaultLimits()
	limits.BlobBytes = -1
	packed := testPackedTarget{limits: limits, open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return nil, errors.New("must not open")
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, PackedContent: packed})
	require.ErrorContains(t, err, "invalid limits")
	_, err = os.Stat(filepath.Join(target, "content", "packs"))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestRestorePackedTargetRejectsPortablePackSubtreeAliasesBeforePublication(t *testing.T) {
	for _, reserved := range []string{"packs", "PACKS", `PaCkS\shard`} {
		t.Run(strings.ReplaceAll(reserved, `\`, "-"), func(t *testing.T) {
			r, app, m, _ := createPackedRestoreFixture(t)
			known, err := r.LoadBlobIndex()
			require.NoError(t, err)
			refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, packstore.PackExt)
			require.NoError(t, err)
			var declined ContentRef
			for _, ref := range refs {
				if ref.Size > declined.Size {
					declined = ref
				}
			}
			id, err := pack.ParseBlobID(declined.Hash)
			require.NoError(t, err)
			packID := known[id].PackID
			sourcePack := r.packPath(packID, packstore.PackExt)
			sourceBytes, err := os.ReadFile(sourcePack)
			require.NoError(t, err)

			target, checkIntact := seedLiveOverwriteTarget(t)
			finalPack := filepath.Join(target, "content", "packs", packID[:2], packID+packstore.PackExt)
			require.NoError(t, os.MkdirAll(filepath.Dir(finalPack), 0o700))
			require.NoError(t, os.WriteFile(finalPack, sourceBytes, 0o600))
			catalogCalled := false
			limits := packstore.DefaultLimits()
			limits.BlobBytes = 25
			packed := testPackedTarget{limits: limits, open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
				catalogCalled = true
				return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
			}}
			restoreApp := contentPathOverrideApp{App: app, override: func(paths map[string][]string) {
				prefix := reserved
				if !strings.ContainsAny(prefix, `/\`) {
					prefix += "/" + packID[:2]
				}
				paths[declined.Hash] = []string{prefix + "/" + packID + packstore.PackExt}
			}}

			_, err = Restore(context.Background(), r, restoreApp, RestoreOptions{
				TargetDir: target, Overwrite: true, PackedContent: packed,
			})
			require.ErrorContains(t, err, "reserved packed-content subtree")
			assert.False(t, catalogCalled)
			got, err := os.ReadFile(finalPack)
			require.NoError(t, err)
			assert.Equal(t, sourceBytes, got)
			checkIntact()
		})
	}
}

func TestRestoreWithoutPackedTargetAllowsHistoricalPackNamedPath(t *testing.T) {
	r, app, m, _ := createPackedRestoreFixture(t)
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, packstore.PackExt)
	require.NoError(t, err)
	ref := refs[0]
	rel := filepath.Join("packs", "historical", ref.Hash)
	restoreApp := contentPathOverrideApp{App: app, override: func(paths map[string][]string) {
		paths[ref.Hash] = []string{rel}
	}}
	target := filepath.Join(t.TempDir(), "restore")

	res, err := Restore(context.Background(), r, restoreApp, RestoreOptions{TargetDir: target})
	require.NoError(t, err)
	assert.Equal(t, m.Attachments.Blobs, res.LooseAttachmentBlobs)
	got, err := os.ReadFile(filepath.Join(target, "content", rel))
	require.NoError(t, err)
	assert.Equal(t, ref.Size, int64(len(got)))
}

func TestRestorePackedTargetCatalogWriteFailureCleansOnlyStagedSidecars(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(_ context.Context, db *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(ctx context.Context, _ []packstore.PackRecord, _ []packstore.Adoption) error {
			var mode string
			require.NoError(t, db.QueryRowContext(ctx, "PRAGMA journal_mode=PERSIST").Scan(&mode))
			_, err := db.ExecContext(ctx, "CREATE TABLE staged_sidecar_probe (id INTEGER)")
			require.NoError(t, err)
			return errors.New("catalog failed after write")
		}), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.ErrorContains(t, err, "catalog failed after write")
	checkIntact()
	entries, err := os.ReadDir(target)
	require.NoError(t, err)
	for _, entry := range entries {
		assert.False(t, strings.HasPrefix(entry.Name(), "app.db.restore-"), entry.Name())
	}
	_, err = os.Stat(filepath.Join(target, "app.db-wal"))
	require.NoError(t, err, "the visible database sidecar must not be cleaned")
}

// Not parallel: this test injects the package-global directory sync hook.
func TestRestorePackedTargetLooseDurabilityFailurePreventsCatalogAuthority(t *testing.T) {
	r, app, m, _ := createPackedRestoreFixture(t)
	known, err := r.LoadBlobIndex()
	require.NoError(t, err)
	refs, _, err := LoadListRefs(r, known, m.Attachments.Lists, nil, packstore.PackExt)
	require.NoError(t, err)
	id, err := pack.ParseBlobID(refs[0].Hash)
	require.NoError(t, err)
	packID := known[id].PackID
	sourceBytes, err := os.ReadFile(r.packPath(packID, packstore.PackExt))
	require.NoError(t, err)
	target, checkIntact := seedLiveOverwriteTarget(t)
	finalPack := filepath.Join(target, "content", "packs", packID[:2], packID+packstore.PackExt)
	require.NoError(t, os.MkdirAll(filepath.Dir(finalPack), 0o700))
	require.NoError(t, os.WriteFile(finalPack, sourceBytes, 0o600))
	catalogCalled := false
	limits := packstore.DefaultLimits()
	limits.BlobBytes = 25
	packed := testPackedTarget{limits: limits, open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		catalogCalled = true
		return nil, errors.New("must not open")
	}}
	originalSync := pack.SyncDir
	pack.SyncDir = func(dir string) error {
		if dir == filepath.Join(target, "content") {
			return fmt.Errorf("injected content durability failure")
		}
		return originalSync(dir)
	}
	t.Cleanup(func() { pack.SyncDir = originalSync })

	_, err = Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, Overwrite: true, PackedContent: packed,
	})
	require.ErrorContains(t, err, "injected content durability failure")
	assert.False(t, catalogCalled)
	checkIntact()
}
