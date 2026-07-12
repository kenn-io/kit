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
	"time"

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
	limits      packstore.Limits
	coordinator *packstore.Coordinator
	acquire     func(context.Context) (*packstore.Lease, error)
	open        func(context.Context, *sql.DB) (packstore.RestoreCatalog, error)
}

func (t testPackedTarget) Limits() packstore.Limits { return t.limits }

func (t testPackedTarget) AcquireRestoreLease(ctx context.Context) (*packstore.Lease, error) {
	if t.acquire != nil {
		return t.acquire(ctx)
	}
	coordinator := t.coordinator
	if coordinator == nil {
		coordinator = defaultTestPackedRestoreCoordinator
	}
	return coordinator.AcquireMutation(ctx)
}

func (t testPackedTarget) OpenRestoreCatalog(ctx context.Context, db *sql.DB) (packstore.RestoreCatalog, error) {
	return t.open(ctx, db)
}

var defaultTestPackedRestoreCoordinator = packstore.NewCoordinator()

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

func TestRestorePackedTargetRejectsInvalidRestoreLeaseBeforePackPublication(t *testing.T) {
	acquireErr := errors.New("acquire restore lease")
	maintenanceCoordinator := packstore.NewCoordinator()
	tests := []struct {
		name    string
		acquire func(context.Context) (*packstore.Lease, error)
		wantErr error
		after   func(*testing.T)
	}{
		{name: "acquisition error", acquire: func(context.Context) (*packstore.Lease, error) {
			return nil, acquireErr
		}, wantErr: acquireErr},
		{name: "nil lease", acquire: func(context.Context) (*packstore.Lease, error) {
			return nil, nil
		}, wantErr: packstore.ErrLeaseReleased},
		{name: "zero lease", acquire: func(context.Context) (*packstore.Lease, error) {
			return &packstore.Lease{}, nil
		}, wantErr: packstore.ErrLeaseReleased},
		{name: "released lease", acquire: func(ctx context.Context) (*packstore.Lease, error) {
			lease, err := packstore.NewCoordinator().AcquireMutation(ctx)
			if err != nil {
				return nil, err
			}
			if err := lease.Release(); err != nil {
				return nil, err
			}
			return lease, nil
		}, wantErr: packstore.ErrLeaseReleased},
		{name: "maintenance lease", acquire: maintenanceCoordinator.AcquireMaintenance,
			wantErr: packstore.ErrWrongLeaseKind, after: func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				lease, err := maintenanceCoordinator.AcquireMutation(ctx)
				require.NoError(t, err, "rejected maintenance lease must be released")
				require.NoError(t, lease.Release())
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, app, _, _ := createPackedRestoreFixture(t)
			target := filepath.Join(t.TempDir(), "restore")
			catalogOpened := false
			packed := testPackedTarget{
				limits:  packstore.DefaultLimits(),
				acquire: tt.acquire,
				open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
					catalogOpened = true
					return nil, errors.New("must not open")
				},
			}

			_, err := Restore(context.Background(), r, app, RestoreOptions{
				TargetDir: target, PackedContent: packed,
			})
			require.ErrorIs(t, err, tt.wantErr)
			assert.False(t, catalogOpened)
			_, err = os.Stat(filepath.Join(target, "content", "packs"))
			require.ErrorIs(t, err, os.ErrNotExist)
			if tt.after != nil {
				tt.after(t)
			}
		})
	}
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

type leaseResult struct {
	lease *packstore.Lease
	err   error
}

// maintenanceQueueContext exposes the point where AcquireMaintenance has
// registered its waiter: the first Err check happens before locking, and the
// second happens after waitingMaintenance is incremented.
type maintenanceQueueContext struct {
	context.Context
	queued   chan struct{}
	errCalls int
}

func newMaintenanceQueueContext() *maintenanceQueueContext {
	return &maintenanceQueueContext{Context: context.Background(), queued: make(chan struct{})}
}

func (c *maintenanceQueueContext) Err() error {
	c.errCalls++
	if c.errCalls == 2 {
		close(c.queued)
	}
	return c.Context.Err()
}

func assertMaintenanceBlocked(t *testing.T, acquired <-chan leaseResult, where string) {
	t.Helper()
	assert.Empty(t, acquired, where)
}

func requireMaintenanceLease(t *testing.T, acquired <-chan leaseResult) *packstore.Lease {
	t.Helper()
	select {
	case result := <-acquired:
		require.NoError(t, result.err)
		require.NotNil(t, result.lease)
		return result.lease
	case <-time.After(time.Second):
		require.Fail(t, "maintenance did not acquire after restore lease release")
		return nil
	}
}

// Not parallel: this test injects the package-global directory sync hook.
func TestRestorePackedTargetHoldsLeaseThroughCatalogProofPublicationAndFinalSync(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	coordinator := packstore.NewCoordinator()
	maintenance := make(chan leaseResult, 1)
	maintenanceCtx := newMaintenanceQueueContext()
	openChecked := false
	replaceChecked := false
	proofChecked := false
	finalSyncChecked := false
	packed := testPackedTarget{limits: packstore.DefaultLimits(), coordinator: coordinator}
	packed.open = func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		go func() {
			lease, err := coordinator.AcquireMaintenance(maintenanceCtx)
			maintenance <- leaseResult{lease: lease, err: err}
		}()
		<-maintenanceCtx.queued
		assertMaintenanceBlocked(t, maintenance, "maintenance acquired during OpenRestoreCatalog")
		openChecked = true
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
			assertMaintenanceBlocked(t, maintenance, "maintenance acquired during ReplaceRestoredPacks")
			replaceChecked = true
			return nil
		}), nil
	}
	restoreApp := proofObservingApp{App: app, beforeStats: func() {
		assertMaintenanceBlocked(t, maintenance, "maintenance acquired during restore proof")
		proofChecked = true
	}}
	originalSync := pack.SyncDir
	pack.SyncDir = func(dir string) error {
		if _, err := os.Stat(filepath.Join(target, app.DBFileName())); err == nil {
			assertMaintenanceBlocked(t, maintenance, "maintenance acquired after database publication or during final sync")
			finalSyncChecked = true
		}
		return originalSync(dir)
	}
	t.Cleanup(func() { pack.SyncDir = originalSync })

	_, err := Restore(context.Background(), r, restoreApp, RestoreOptions{
		TargetDir: target, PackedContent: packed,
	})
	require.NoError(t, err)
	assert.True(t, openChecked)
	assert.True(t, replaceChecked)
	assert.True(t, proofChecked)
	assert.True(t, finalSyncChecked)
	require.NoError(t, requireMaintenanceLease(t, maintenance).Release())
}

// Not parallel: this test injects the package-global directory sync hook.
func TestRestorePackedTargetReleasesLeaseAfterPostPublicationFailure(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	coordinator := packstore.NewCoordinator()
	packed := testPackedTarget{
		limits:      packstore.DefaultLimits(),
		coordinator: coordinator,
		open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
			return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
				return nil
			}), nil
		},
	}
	publicationErr := errors.New("final sync after database publication")
	originalSync := pack.SyncDir
	pack.SyncDir = func(dir string) error {
		if _, err := os.Stat(filepath.Join(target, app.DBFileName())); err == nil {
			return publicationErr
		}
		return originalSync(dir)
	}
	t.Cleanup(func() { pack.SyncDir = originalSync })

	_, err := Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, PackedContent: packed,
	})
	require.ErrorIs(t, err, publicationErr)
	acquireCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	maintenance, acquireErr := coordinator.AcquireMaintenance(acquireCtx)
	require.NoError(t, acquireErr)
	require.NoError(t, maintenance.Release())
}

func TestRestorePackedTargetJoinsReleaseErrorWithPrimaryError(t *testing.T) {
	r, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	coordinator := packstore.NewCoordinator()
	var restoreLease *packstore.Lease
	primaryErr := errors.New("catalog publication failed")
	packed := testPackedTarget{
		limits: packstore.DefaultLimits(),
		acquire: func(ctx context.Context) (*packstore.Lease, error) {
			lease, err := coordinator.AcquireMutation(ctx)
			restoreLease = lease
			return lease, err
		},
		open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
			return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error {
				require.NoError(t, restoreLease.Release())
				return primaryErr
			}), nil
		},
	}

	_, err := Restore(context.Background(), r, app, RestoreOptions{
		TargetDir: target, PackedContent: packed,
	})
	require.ErrorIs(t, err, primaryErr)
	require.ErrorIs(t, err, packstore.ErrLeaseReleased)
	assert.ErrorContains(t, err, "releasing packed restore lease")
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

func installStagedCatalogFileHooks(t *testing.T) {
	t.Helper()
	originalOpen := openStagedCatalogFile
	originalSync := syncStagedCatalogFile
	originalClose := closeStagedCatalogFile
	t.Cleanup(func() {
		openStagedCatalogFile = originalOpen
		syncStagedCatalogFile = originalSync
		closeStagedCatalogFile = originalClose
	})
}

func stagedCatalogPath(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	var sequence int
	var name, filename string
	require.NoError(t, db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&sequence, &name, &filename))
	return filename
}

// Not parallel: these tests inject package-global staged catalog file hooks.
func TestRestorePackedTargetSyncsClosedStagedCatalogBeforeProofAndPublication(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target := filepath.Join(t.TempDir(), "restore")
	var catalogDB *sql.DB
	var stagedPath string
	var syncedFile *os.File
	synced := false
	closed := false
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(ctx context.Context, db *sql.DB) (packstore.RestoreCatalog, error) {
		catalogDB = db
		stagedPath = stagedCatalogPath(t, ctx, db)
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}
	originalSync := syncStagedCatalogFile
	syncStagedCatalogFile = func(file *os.File) error {
		require.Error(t, catalogDB.PingContext(context.Background()), "SQLite must be closed before file sync")
		openedInfo, err := file.Stat()
		require.NoError(t, err)
		stagedInfo, err := os.Stat(stagedPath)
		require.NoError(t, err)
		require.True(t, os.SameFile(openedInfo, stagedInfo), "sync handle must name the staged database")
		syncedFile = file
		synced = true
		return originalSync(file)
	}
	originalClose := closeStagedCatalogFile
	closeStagedCatalogFile = func(file *os.File) error {
		err := originalClose(file)
		closed = true
		return err
	}
	proofApp := proofObservingApp{App: app, beforeStats: func() {
		require.True(t, synced, "staged database must be synced before proof")
		require.True(t, closed, "synced staged database handle must be closed before proof")
		_, err := syncedFile.Stat()
		require.Error(t, err)
	}}

	_, err := Restore(context.Background(), r, proofApp, RestoreOptions{TargetDir: target, PackedContent: packed})
	require.NoError(t, err)
	assert.True(t, synced)
	assert.True(t, closed)
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetCatalogReplacementFailureDoesNotSyncStagedCatalog(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	replaceErr := errors.New("catalog replacement failed")
	syncCalled := false
	syncStagedCatalogFile = func(*os.File) error {
		syncCalled = true
		return nil
	}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return replaceErr }), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorIs(t, err, replaceErr)
	assert.False(t, syncCalled)
	checkIntact()
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetStagedCatalogSyncFailurePreventsProofAndPublication(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	syncErr := errors.New("staged catalog sync failed")
	var syncedFile *os.File
	closed := false
	syncStagedCatalogFile = func(file *os.File) error {
		syncedFile = file
		return syncErr
	}
	originalClose := closeStagedCatalogFile
	closeStagedCatalogFile = func(file *os.File) error {
		err := originalClose(file)
		closed = true
		return err
	}
	proofCalled := false
	proofApp := proofObservingApp{App: app, beforeStats: func() { proofCalled = true }}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err := Restore(context.Background(), r, proofApp, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorIs(t, err, syncErr)
	assert.False(t, proofCalled)
	assert.True(t, closed)
	_, statErr := syncedFile.Stat()
	require.Error(t, statErr)
	checkIntact()
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetJoinsStagedCatalogSyncAndCloseFailures(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	syncErr := errors.New("staged catalog sync failed")
	closeErr := errors.New("staged catalog close failed")
	syncStagedCatalogFile = func(*os.File) error { return syncErr }
	closeStagedCatalogFile = func(file *os.File) error {
		return errors.Join(file.Close(), closeErr)
	}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorIs(t, err, syncErr)
	require.ErrorIs(t, err, closeErr)
	checkIntact()
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetStagedCatalogCloseFailurePreventsProofAndPublication(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	closeErr := errors.New("staged catalog close failed")
	var closedFile *os.File
	closeStagedCatalogFile = func(file *os.File) error {
		closedFile = file
		require.NoError(t, file.Close())
		return closeErr
	}
	proofCalled := false
	proofApp := proofObservingApp{App: app, beforeStats: func() { proofCalled = true }}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err := Restore(context.Background(), r, proofApp, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorIs(t, err, closeErr)
	assert.False(t, proofCalled)
	_, statErr := closedFile.Stat()
	require.Error(t, statErr)
	checkIntact()
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetRejectsNonRegularStagedCatalogBeforeOpen(t *testing.T) {
	for _, tt := range []struct {
		name  string
		plant func(*testing.T, string)
	}{
		{name: "symlink", plant: func(t *testing.T, staged string) {
			victim := filepath.Join(t.TempDir(), "victim.db")
			require.NoError(t, os.WriteFile(victim, []byte("victim"), 0o600))
			require.NoError(t, os.Remove(staged))
			if err := os.Symlink(victim, staged); err != nil {
				t.Skip("symlinks not supported on this platform")
			}
		}},
		{name: "directory", plant: func(t *testing.T, staged string) {
			require.NoError(t, os.Remove(staged))
			require.NoError(t, os.Mkdir(staged, 0o700))
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			installStagedCatalogFileHooks(t)
			target := t.TempDir()
			staged := filepath.Join(target, "staged.db")
			require.NoError(t, os.WriteFile(staged, []byte("staged"), 0o600))
			root, err := os.OpenRoot(target)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, root.Close()) })
			openCalled := false
			syncCalled := false
			openStagedCatalogFile = func(*os.Root, string) (*os.File, error) {
				openCalled = true
				return nil, errors.New("must not open")
			}
			syncStagedCatalogFile = func(*os.File) error {
				syncCalled = true
				return errors.New("must not sync")
			}
			tt.plant(t, staged)
			state := restoreState{root: root, dbRead: "staged.db"}
			err = state.syncAndCloseStagedCatalog()
			require.ErrorContains(t, err, "regular file")
			assert.False(t, openCalled)
			assert.False(t, syncCalled)
		})
	}
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetRejectsOpenedStagedCatalogIdentityMismatchAndJoinsCloseFailure(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	closeErr := errors.New("closing mismatched staged catalog")
	syncCalled := false
	closed := false
	openStagedCatalogFile = func(root *os.Root, _ string) (*os.File, error) {
		return root.OpenFile("different-regular.db", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	}
	syncStagedCatalogFile = func(*os.File) error {
		syncCalled = true
		return nil
	}
	closeStagedCatalogFile = func(file *os.File) error {
		closed = true
		return errors.Join(file.Close(), closeErr)
	}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err := Restore(context.Background(), r, app, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorContains(t, err, "identity")
	require.ErrorIs(t, err, closeErr)
	assert.False(t, syncCalled)
	assert.True(t, closed)
	checkIntact()
}

// Not parallel: this test injects package-global staged catalog file hooks.
func TestRestorePackedTargetRejectsStagedCatalogReplacedDuringSync(t *testing.T) {
	installStagedCatalogFileHooks(t)
	r, app, _, _ := createPackedRestoreFixture(t)
	target, checkIntact := seedLiveOverwriteTarget(t)
	var root *os.Root
	var stagedName string
	var syncedFile *os.File
	closed := false
	originalOpen := openStagedCatalogFile
	openStagedCatalogFile = func(openRoot *os.Root, name string) (*os.File, error) {
		root, stagedName = openRoot, name
		return originalOpen(openRoot, name)
	}
	originalSync := syncStagedCatalogFile
	syncStagedCatalogFile = func(file *os.File) error {
		syncedFile = file
		if err := originalSync(file); err != nil {
			return err
		}
		require.NoError(t, root.Rename(stagedName, stagedName+".aside"))
		replacement, err := root.OpenFile(stagedName, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		require.NoError(t, err)
		require.NoError(t, replacement.Close())
		return nil
	}
	originalClose := closeStagedCatalogFile
	closeStagedCatalogFile = func(file *os.File) error {
		err := originalClose(file)
		closed = true
		return err
	}
	proofCalled := false
	proofApp := proofObservingApp{App: app, beforeStats: func() { proofCalled = true }}
	packed := testPackedTarget{limits: packstore.DefaultLimits(), open: func(context.Context, *sql.DB) (packstore.RestoreCatalog, error) {
		return restoreCatalogFunc(func(context.Context, []packstore.PackRecord, []packstore.Adoption) error { return nil }), nil
	}}

	_, err := Restore(context.Background(), r, proofApp, RestoreOptions{TargetDir: target, Overwrite: true, PackedContent: packed})
	require.ErrorContains(t, err, "identity")
	assert.False(t, proofCalled)
	assert.True(t, closed)
	_, statErr := syncedFile.Stat()
	require.Error(t, statErr)
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
