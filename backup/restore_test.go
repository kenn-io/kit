package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.kenn.io/kit/pack"
)

// TestSyncRestoredTreeCoversCreatedAncestors pins the durability pass for a
// restore target whose ancestors did not exist before the restore: the sync
// must cover the restored tree deepest-first, then climb through the created
// ancestors to the pre-existing ceiling that received the topmost new entry.
// Not parallel: it stubs the package-level pack.SyncDir hook.
func TestSyncRestoredTreeCoversCreatedAncestors(t *testing.T) {
	require := require.New(t)
	base := t.TempDir()
	target := filepath.Join(base, "a", "b", "out")

	ceiling := restoreSyncCeiling(target)
	require.Equal(base, ceiling, "the ceiling is the deepest ancestor existing before creation")

	require.NoError(os.MkdirAll(filepath.Join(target, "content", "aa"), 0o700))
	require.Equal(target, restoreSyncCeiling(target),
		"a target that already exists is its own ceiling; nothing above it gains entries")

	var synced []string
	origSyncDir := pack.SyncDir
	pack.SyncDir = func(dir string) error {
		synced = append(synced, dir)
		return nil
	}
	t.Cleanup(func() { pack.SyncDir = origSyncDir })

	require.NoError(syncRestoredTree(target, ceiling))
	require.Equal([]string{
		filepath.Join(target, "content", "aa"),
		filepath.Join(target, "content"),
		target,
		filepath.Join(base, "a", "b"),
		filepath.Join(base, "a"),
		base,
	}, synced, "deepest first: every directory's entry is durable in its parent before that parent syncs")

	synced = nil
	require.NoError(syncRestoredTree(target, target))
	require.Equal(target, synced[len(synced)-1],
		"with a pre-existing target the sync stops at the target itself")
}

func fileSHA256(t *testing.T, path string) [32]byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return sha256.Sum256(data)
}

// snapshotDirHashes maps every regular file under root (relative path) to
// its content hash, for whole-tree equality comparisons.
func snapshotDirHashes(t *testing.T, root string) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		require.NoError(t, err)
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		require.NoError(t, err)
		out[rel] = fileSHA256(t, path)
		return nil
	})
	require.NoError(t, err)
	return out
}

// TestRestoreReproducesArchiveByteForByte is the restore proof's proof: a
// restored snapshot's database is byte-identical to the live database file
// as it existed at capture time, attachments and extras land byte-identical
// in the live layout, and this holds for a parent snapshot restored from an
// incremental chain, not just the latest.
func TestRestoreReproducesArchiveByteForByte(t *testing.T) {
	require := require.New(t)
	assert := assert.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, writer := seedBackupFixture(t)
	cacheDir := t.TempDir()

	// An extras file rides along (the deletions dir is always captured).
	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, []byte(`{"id":"manifest-1"}`), 0o640))
	// WriteFile's mode is umask-filtered; pin the mode so the
	// capture-and-restore round trip has a distinctive value to preserve.
	require.NoError(os.Chmod(deletionsPath, 0o640))

	m1, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	// Create checkpoint-truncated the WAL inside the freeze and nothing has
	// written since, so the on-disk file is exactly the captured state.
	dbAtSnap1, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Mutate the archive and take an incremental child snapshot. One added
	// attachment lives in the loose layout, one under an importer-style
	// namespaced storage path — restore must reproduce both placements. The
	// namespace deliberately starts with "http": only http:// and https://
	// URLs are excluded from capture, never local paths sharing the prefix.
	_, err = writer.ExecContext(ctx, `INSERT INTO notes (created_at) VALUES ('2026-03-01T00:00:00Z')`)
	require.NoError(err)
	newRef := writeLooseAttachment(t, attachmentsDir, []byte("attachment added after snapshot 1"))
	nsRef := writeNamespacedAttachment(t, attachmentsDir, "http-cache", []byte("namespaced attachment"))
	_, err = writer.ExecContext(ctx,
		`INSERT INTO blobs (content_hash, storage_path, size, preview_hash, preview_path)
		 VALUES (?, ?, ?, '', ''), (?, ?, ?, '', '')`,
		newRef.Hash, newRef.Hash[:2]+"/"+newRef.Hash, newRef.Size,
		nsRef.Hash, nsRef.StoragePath, nsRef.Size)
	require.NoError(err)
	m2, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)
	require.Equal(m1.SnapshotID, m2.ParentID)
	dbAtSnap2, err := os.ReadFile(dbPath)
	require.NoError(err)

	// Restore the latest snapshot and compare byte-for-byte.
	target2 := filepath.Join(t.TempDir(), "restore-2")
	var events []ProgressEvent
	res2, err := Restore(ctx, r, newTestApp(), RestoreOptions{
		TargetDir: target2,
		Progress:  func(ev ProgressEvent) { events = append(events, ev) },
	})
	require.NoError(err)
	assert.Equal(m2.SnapshotID, res2.SnapshotID)
	restored2, err := os.ReadFile(res2.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap2, restored2),
		"restored database must be byte-identical to the live file at capture time")
	assert.Equal(m2.Attachments.Blobs, res2.AttachmentBlobs)
	assert.Equal(m2.Attachments.BlobBytes, res2.AttachmentBytes)

	// Attachments land in the loose layout with matching bytes.
	assert.Equal(snapshotDirHashes(t, attachmentsDir), snapshotDirHashes(t, filepath.Join(target2, "content")))

	// Extras land at their captured relative path with mode preserved.
	restoredDeletions := filepath.Join(target2, "deletions", "manifest-1.json")
	assert.Equal(fileSHA256(t, deletionsPath), fileSHA256(t, restoredDeletions))
	if runtime.GOOS != "windows" {
		// Windows has no POSIX permission bits — Stat reports 0666 for any
		// writable file — so the exact-mode round trip is POSIX-only.
		info, err := os.Stat(restoredDeletions)
		require.NoError(err)
		assert.Equal(os.FileMode(0o640), info.Mode().Perm())
	}

	// Restoring the PARENT from the incremental chain reproduces the older
	// state, not the current one.
	target1 := filepath.Join(t.TempDir(), "restore-1")
	res1, err := Restore(ctx, r, newTestApp(), RestoreOptions{SnapshotID: m1.SnapshotID, TargetDir: target1})
	require.NoError(err)
	restored1, err := os.ReadFile(res1.DBPath)
	require.NoError(err)
	require.True(bytes.Equal(dbAtSnap1, restored1),
		"restoring the parent snapshot must reproduce the pre-mutation database")
	_, err = os.Stat(filepath.Join(target1, "content", newRef.Hash[:2], newRef.Hash))
	require.ErrorIs(err, os.ErrNotExist,
		"an attachment added after snapshot 1 must not appear in snapshot 1's restore")

	// Progress: the db, attachments, and proof stages all completed.
	final := map[ProgressStage]ProgressEvent{}
	for _, ev := range events {
		if ev.Final {
			final[ev.Stage] = ev
		}
	}
	for _, stage := range []ProgressStage{ProgressStageRestoreDB, ProgressStageAttachments, ProgressStageExtras, ProgressStageProof} {
		require.Contains(final, stage)
		assert.Equal(final[stage].Done, final[stage].Total, "stage %s must finish complete", stage)
	}
}

// collidingContentPathApp maps every content hash to one shared restore path,
// modeling an App whose derivation returns distinct blobs at the same relative
// path.
type collidingContentPathApp struct{ App }

func (a collidingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{"shared/collision"}
	}
	return out, nil
}

// TestRestoreRejectsCollidingAttachmentPaths pins the path-collision guard: two
// different content hashes claiming the same restore path must fail the restore
// rather than have the parallel writer's temp-then-rename clobber one blob with
// the other and still report success.
func TestRestoreRejectsCollidingAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	_, err = Restore(ctx, r, collidingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "two different attachments")
	require.ErrorContains(err, "shared/collision")
}

// caseFoldingContentPathApp assigns two distinct content hashes to restore
// paths that differ only in case ("A/b" and "a/b"). The assignment is keyed on
// sorted hash order so it is stable across the randomized map iteration.
type caseFoldingContentPathApp struct{ App }

func (a caseFoldingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	hashes := make([]string, 0, len(paths))
	for hash := range paths {
		hashes = append(hashes, hash)
	}
	sort.Strings(hashes)
	rels := []string{"A/b", "a/b"}
	out := make(map[string][]string, len(paths))
	for i, hash := range hashes {
		out[hash] = []string{rels[i%len(rels)]}
	}
	return out, nil
}

// TestRestoreRejectsCaseFoldingAttachmentPaths pins that two distinct blobs
// whose restore paths differ only in case collide on every platform: a
// case-insensitive filesystem would clobber one with the other, so restore
// rejects the pair up front rather than reporting a lossy success.
func TestRestoreRejectsCaseFoldingAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	_, err = Restore(ctx, r, caseFoldingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "case-folded key")
	require.ErrorContains(err, "two different attachments")
}

// badContentPathApp maps every content hash to one fixed restore path, for
// exercising restore's validation of paths the restored database records.
type badContentPathApp struct {
	App
	path string
}

func (a badContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{a.path}
	}
	return out, nil
}

// TestRestoreRejectsBadAttachmentPaths pins that restore refuses attachment
// paths a tampered or buggy database could record: a component ending in a
// dot or space (which Windows trims, aliasing a different name), and any "."
// or ".." component — including paths like "." or "safe/.." that pass
// filepath.IsLocal yet clean to the content directory itself, which restore
// must never write as a file. All are rejected on every platform before any
// write.
func TestRestoreRejectsBadAttachmentPaths(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	for _, path := range []string{"blobs./file", ".", "safe/..", "safe/../x"} {
		_, err = Restore(ctx, r, badContentPathApp{App: newTestApp(), path: path}, RestoreOptions{
			TargetDir: filepath.Join(t.TempDir(), "restore"),
		})
		require.ErrorContains(err, "ending in a dot or space", "path %q", path)
	}
}

// TestRestoreRefusesSymlinkTarget pins that a restore target whose final
// component is a symlink is refused: os.ReadDir and os.OpenRoot both follow it,
// so restore would otherwise materialize the archive under the link's target.
func TestRestoreRefusesSymlinkTarget(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	realDir := t.TempDir()
	link := filepath.Join(t.TempDir(), "target-link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: link})
	require.ErrorContains(err, "is a symlink")

	entries, err := os.ReadDir(realDir)
	require.NoError(err)
	require.Empty(entries, "restore must refuse before writing anything under the symlink's target")
}

func TestRestoreRefusesNonEmptyTargetWithoutOverwrite(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	require.NoError(os.WriteFile(filepath.Join(target, "existing.txt"), []byte("x"), 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
	require.ErrorContains(err, "not empty")

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
}

// TestRestoreOverwriteRemovesStaleDBSidecars pins the --overwrite hazard: a
// stale -wal/-shm pair or rollback journal left next to the restored database
// would be replayed over the proven bytes on its first normal SQLite open, so
// overwrite must remove them even though it merges the rest of the tree.
func TestRestoreOverwriteRemovesStaleDBSidecars(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	for _, name := range []string{"app.db", "app.db-wal", "app.db-shm", "app.db-journal"} {
		require.NoError(os.WriteFile(filepath.Join(target, name), []byte("stale "+name), 0o600))
	}
	unrelated := filepath.Join(target, "keep-me.txt")
	require.NoError(os.WriteFile(unrelated, []byte("survives the merge"), 0o600))

	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)
	for _, name := range []string{"app.db-wal", "app.db-shm", "app.db-journal"} {
		_, err := os.Stat(filepath.Join(target, name))
		require.ErrorIs(err, os.ErrNotExist, "stale sidecar %s must not survive an overwrite restore", name)
	}
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath),
		"the stale database must be fully replaced, not merged")
	_, err = os.Stat(unrelated)
	require.NoError(err, "overwrite merges: unrelated files stay in place")
}

// TestRestoreOverwriteSweepsOrphanedTempFiles pins the crash-recovery sweep: a
// restore killed mid-run leaves a <db>.restore-<ulid> staging temp at the
// target root, and a later --overwrite rerun must remove it rather than leave
// it to accumulate.
func TestRestoreOverwriteSweepsOrphanedTempFiles(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	target := t.TempDir()
	orphan := filepath.Join(target, "app.db.restore-"+pack.NewPackID())
	require.NoError(os.WriteFile(orphan, []byte("half-written restore"), 0o600))
	// A file that merely shares the prefix but is not a valid temp name must
	// survive: the sweep is scoped to the exact <db>.restore-<ulid> shape.
	bystander := filepath.Join(target, "app.db.restore-not-a-ulid")
	require.NoError(os.WriteFile(bystander, []byte("keep me"), 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)

	_, statErr := os.Stat(orphan)
	require.ErrorIs(statErr, os.ErrNotExist, "orphaned restore temp must be swept on overwrite")
	_, statErr = os.Stat(bystander)
	require.NoError(statErr, "a non-temp file sharing the prefix must survive")
}

// TestRestoreTargetPathWithURISyntax pins the proof DSN construction: a
// target path containing '?' or '#' must reach SQLite as a path, not be
// misparsed as URI query/fragment syntax.
func TestRestoreTargetPathWithURISyntax(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	// '?' is illegal in Windows filenames, so exercise it only where the
	// filesystem allows it; '#' and space are legal everywhere.
	dirName := "odd? dir#name"
	if runtime.GOOS == "windows" {
		dirName = "odd dir#name"
	}
	target := filepath.Join(t.TempDir(), dirName)
	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoredDBDSNDrivePath pins the Windows DSN shape: a drive-letter
// path must be rooted with a slash so SQLite's URI parser reads it as a
// path rather than a URI authority. Windows-only: elsewhere filepath.Abs
// treats a drive-letter path as relative.
func TestRestoredDBDSNDrivePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-letter paths are only absolute on Windows")
	}
	assert.Equal(t, "file:///C:/Users/x%20y/app.db?immutable=1&mode=ro",
		restoredDBDSN(`C:\Users\x y\app.db`))
}

// TestRestoredDBDSNRelativePath pins that a relative path resolves against
// the working directory instead of being rooted at "/".
func TestRestoredDBDSNRelativePath(t *testing.T) {
	require := require.New(t)
	t.Chdir(t.TempDir())
	cwd, err := os.Getwd()
	require.NoError(err)

	rel := filepath.Join("out", "app.db")
	dsn := restoredDBDSN(rel)
	require.Equal(restoredDBDSN(filepath.Join(cwd, rel)), dsn,
		"relative and absolute forms of the same path must produce the same DSN")
	require.NotContains(dsn, "file:///out",
		"a relative path must not be rooted at /")
}

// TestRestoreRelativeTarget pins that a relative --target works end to end:
// the proof DSN must resolve the restored database against the working
// directory, not root the relative path at "/".
func TestRestoreRelativeTarget(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	t.Chdir(t.TempDir())
	res, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: "restore-out"})
	require.NoError(err)
	require.Equal(fileSHA256(t, dbPath), fileSHA256(t, res.DBPath))
}

// TestRestoreProofCatchesManifestStatsMismatch proves the proof fires: a
// self-consistent manifest (valid content-derived ID) whose recorded stats
// disagree with the captured pages must fail the restore, not pass silently.
func TestRestoreProofCatchesManifestStatsMismatch(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	path := r.Path(snapshotsDirName, m.SnapshotID+manifestExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	var doctored Manifest
	require.NoError(json.Unmarshal(data, &doctored))
	bumped := mustParseStats(t, doctored.Stats)
	bumped.Notes++
	doctored.Stats, err = json.Marshal(bumped)
	require.NoError(err)
	createdAt, err := time.Parse(time.RFC3339, doctored.CreatedAt)
	require.NoError(err)
	forgedID, err := ComputeSnapshotID(createdAt, &doctored)
	require.NoError(err)
	doctored.SnapshotID = forgedID
	out, err := json.MarshalIndent(&doctored, "", "  ")
	require.NoError(err)
	require.NoError(os.Remove(path))
	require.NoError(os.WriteFile(r.Path(snapshotsDirName, forgedID+manifestExt), out, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.ErrorContains(err, "do not match manifest stats")
}

func TestRestoreDetectsCorruptPack(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	m, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	packID := m.NewPacks[0]
	path := r.Path("packs", packID[:2], packID+testPackExt)
	data, err := os.ReadFile(path)
	require.NoError(err)
	data[len(data)/3] ^= 0x01
	require.NoError(os.WriteFile(path, data, 0o600))

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: filepath.Join(t.TempDir(), "restore")})
	require.Error(err, "a corrupted pack must fail the restore, never produce an unverified tree")
}

// TestRestoreJobsSerialMatchesParallel pins the --jobs contract for restore:
// serial and parallel runs produce byte-identical trees.
func TestRestoreJobsSerialMatchesParallel(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	targets := map[int]string{1: filepath.Join(t.TempDir(), "serial"), 8: filepath.Join(t.TempDir(), "parallel")}
	trees := map[int]map[string][32]byte{}
	for jobs, target := range targets {
		_, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Jobs: jobs})
		require.NoError(err)
		trees[jobs] = snapshotDirHashes(t, target)
	}
	require.Equal(trees[1], trees[8])
}

// TestWriteRunRejectsOverflowingBlobOffset pins the subtraction-based bounds
// check: BlobOffset comes from a decoded page-map object, and a huge value
// must produce a restore error, not wrap the addition-based comparison and
// panic on the slice.
func TestWriteRunRejectsOverflowingBlobOffset(t *testing.T) {
	require := require.New(t)
	st := &restoreState{progress: newProgressEmitter(nil)}
	f, err := os.Create(filepath.Join(t.TempDir(), "db"))
	require.NoError(err)
	defer func() { _ = f.Close() }()

	raw := make([]byte, 4096)
	hm := &PageHashMap{PageSize: 4096, PageCount: 1}
	for _, offset := range []uint64{math.MaxUint64 - 100, math.MaxUint64, 4097} {
		err := st.writeRun(f, raw, blobID("b"), PageRun{StartPage: 0, PageCount: 1, BlobOffset: offset}, 4096, hm)
		require.ErrorContains(err, "overruns blob", "offset %d", offset)
	}
}

// TestRestoreRefusesSymlinkEscapeInTarget pins that restore never writes
// through a symlink planted inside the target: with Overwrite set, a symlink at
// a restored extras path that points outside the target must be replaced, not
// followed, so a file outside the target is never truncated or rewritten.
func TestRestoreRefusesSymlinkEscapeInTarget(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	// An extras file rides along in the snapshot (the deletions dir is captured).
	extrasBody := []byte(`{"id":"manifest-1"}`)
	deletionsPath := filepath.Join(dataDir, "deletions", "manifest-1.json")
	require.NoError(os.MkdirAll(filepath.Dir(deletionsPath), 0o700))
	require.NoError(os.WriteFile(deletionsPath, extrasBody, 0o600))

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	// A victim file outside the restore target.
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.txt")
	sentinel := []byte("must survive the restore untouched")
	require.NoError(os.WriteFile(victim, sentinel, 0o600))

	// Pre-plant, inside the target, a symlink at the extras path that points at
	// the victim; restore runs with Overwrite so the preexisting tree is merged.
	target := filepath.Join(t.TempDir(), "restore")
	require.NoError(os.MkdirAll(filepath.Join(target, "deletions"), 0o700))
	link := filepath.Join(target, "deletions", "manifest-1.json")
	if err := os.Symlink(victim, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	_, err = Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target, Overwrite: true})
	require.NoError(err)

	got, err := os.ReadFile(victim)
	require.NoError(err)
	require.Equal(sentinel, got,
		"a file outside the target must never be written through a symlink")

	info, err := os.Lstat(link)
	require.NoError(err)
	require.Equal(os.FileMode(0), info.Mode()&os.ModeSymlink,
		"the planted symlink must have been replaced by a real file")
	restored, err := os.ReadFile(link)
	require.NoError(err)
	require.Equal(extrasBody, restored)
}

// TestRestoreRefusesSymlinkTargetWithTrailingSeparator pins that the
// symlinked-target guard cannot be sidestepped by addressing the link with a
// trailing separator: POSIX resolves "link/" and "link/." through the symlink
// before lstat, so without normalization the leaf check would report a real
// directory and the whole restore would be redirected into the link's
// destination.
func TestRestoreRefusesSymlinkTargetWithTrailingSeparator(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, t.TempDir()))
	require.NoError(err)

	base := t.TempDir()
	real := filepath.Join(base, "real")
	require.NoError(os.Mkdir(real, 0o700))
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks not supported on this platform")
	}

	for _, target := range []string{link, link + "/", link + "/."} {
		_, err := Restore(ctx, r, newTestApp(), RestoreOptions{TargetDir: target})
		require.ErrorContains(err, "is a symlink", "target %q", target)
	}
	entries, err := os.ReadDir(real)
	require.NoError(err)
	require.Empty(entries, "nothing may be restored through the symlink")
}

// TestCheckExtrasCollisionsRejectsAliasingPaths pins the extras pre-pass:
// two entries that resolve to one file — an exact duplicate, a case-folded
// alias, or a lexically distinct spelling of the same cleaned path — must
// fail the restore before anything is written, on every platform.
func TestCheckExtrasCollisionsRejectsAliasingPaths(t *testing.T) {
	require := require.New(t)

	require.NoError(checkExtrasCollisions([]ExtrasEntry{
		{Path: "tokens/a.json"}, {Path: "config.toml"}, {Path: "deletions/x.json"},
	}))

	err := checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/A"}, {Path: "tokens/a"}})
	require.ErrorContains(err, "collide under case-folded key")

	err = checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/a"}, {Path: "tokens/a"}})
	require.ErrorContains(err, `lists path "tokens/a" twice`)

	err = checkExtrasCollisions([]ExtrasEntry{{Path: "tokens/./a"}, {Path: "tokens/a"}})
	require.ErrorContains(err, "collide under case-folded key")
}

// TestVerifyHeldTargetDetectsReplacedTarget pins the re-verification that
// guards the proof's path-based SQLite opens: once the target directory is
// renamed aside and an impostor directory (with its own database file) is
// planted at the same path, verifyHeldTarget must fail rather than let the
// proof run against a database the held root does not contain.
func TestVerifyHeldTargetDetectsReplacedTarget(t *testing.T) {
	require := require.New(t)
	base := t.TempDir()
	target := filepath.Join(base, "restore")
	require.NoError(os.Mkdir(target, 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, "app.db"), []byte("real"), 0o600))

	root, err := openRestoreRoot(target)
	require.NoError(err)
	defer func() { _ = root.Close() }()
	st := &restoreState{root: root, target: target}

	require.NoError(st.verifyHeldTarget("app.db"),
		"the untouched target must verify against its own root")

	if err := os.Rename(target, filepath.Join(base, "moved-aside")); err != nil {
		// Windows refuses to rename a directory somebody holds open, which
		// also forecloses the replacement this guard detects.
		t.Skip("cannot rename a directory with an open handle on this platform")
	}
	require.NoError(os.Mkdir(target, 0o700))
	require.NoError(os.WriteFile(filepath.Join(target, "app.db"), []byte("impostor"), 0o600))

	err = st.verifyHeldTarget("app.db")
	require.ErrorContains(err, "replaced during restore")

	// A symlink swapped in at the target path must be named as such.
	require.NoError(os.RemoveAll(target))
	if err := os.Symlink(filepath.Join(base, "moved-aside"), target); err != nil {
		t.Skip("symlinks not supported on this platform")
	}
	err = st.verifyHeldTarget("app.db")
	require.ErrorContains(err, "replaced with a symlink")
}

func TestRestoreExtrasEntryRejectsEscapingPaths(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}

	for _, path := range []string{"", "/etc/passwd", "../outside", "a/../../outside", ".."} {
		err := st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "escapes the restore target", "path %q", path)
	}
}

// TestRestoreExtrasEntryRejectsArchiveOverlap pins that a tampered extras
// tree cannot overwrite outputs restore already produced and proved: the
// database, its SQLite sidecars, and the attachments tree are off limits,
// case-insensitively (the default macOS filesystem folds case).
func TestRestoreExtrasEntryRejectsArchiveOverlap(t *testing.T) {
	require := require.New(t)
	st := &restoreState{}

	for _, path := range []string{
		"app.db", "APP.DB", "app.db-wal", "app.db-shm", "app.db-journal",
		"content/aa/aa11", "Content/aa/aa11", "content",
	} {
		err := st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// Traversal that lexically resolves onto a reserved path must be caught
	// too: the raw first segment looks safe, but filepath.Join cleans the
	// ".." away before the write.
	for _, path := range []string{
		"safe/../app.db", "safe/../APP.DB-wal", "safe/../content/aa/aa11",
	} {
		err := st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "overlaps restored archive content", "path %q", path)
	}

	// Components ending in a dot or space are rejected outright: on Windows
	// "content." and "content " resolve to the reserved content dir, aliasing
	// it past the folded comparison, and such names are pathological on every
	// platform. The guard fires for the first component and for nested ones.
	for _, path := range []string{
		"content.", "content ", "content./aa/aa11", "app.db.", "app.db ",
		"safe/dir./file", "safe/dir /file",
	} {
		err := st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: path, Blob: blobID("x").String()})
		require.ErrorContains(err, "component ending in a dot or space", "path %q", path)
	}

	// A path that cleans to the target directory itself is rejected outright.
	err := st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: "safe/..", Blob: blobID("x").String()})
	require.ErrorContains(err, "escapes the restore target")

	// Legitimate extras still restore fine (proven end-to-end elsewhere);
	// here just confirm the reserved-name check does not reject them.
	err = st.restoreExtrasEntry(newTestApp(), ExtrasEntry{Path: "deletions/manifest-1.json", Blob: "not-a-blob"})
	require.NotContains(err.Error(), "overlaps restored archive content")
}

// escapingContentPathApp wraps another App but rewrites every path
// RestoredContentPaths reports to a traversal path, modeling an App
// implementation whose own derivation does not validate what it returns
// (unlike testApp, which does) so this test exercises the engine's own guard
// directly, regardless of which attachment the engine happens to process
// first.
type escapingContentPathApp struct{ App }

func (a escapingContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths))
	for hash := range paths {
		out[hash] = []string{"../escape"}
	}
	return out, nil
}

// TestRestoreAttachmentsRejectsEscapingContentPath pins the engine-side guard
// on App.RestoredContentPaths: a path with the same untrusted, restored-DB
// provenance as extras tree entries must be rejected before it is joined
// into the content directory, even when nothing else about the snapshot is
// invalid.
func TestRestoreAttachmentsRejectsEscapingContentPath(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = Restore(ctx, r, escapingContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "escapes the content directory")
	require.ErrorContains(err, "../escape")
}

// extraContentPathApp wraps another App but reports one content hash beyond
// those the snapshot's attachment lists carry, modeling a restored database
// that references a blob no attachment list names.
type extraContentPathApp struct{ App }

func (a extraContentPathApp) RestoredContentPaths(ctx context.Context, db *sql.DB) (map[string][]string, error) {
	paths, err := a.App.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string, len(paths)+1)
	maps.Copy(out, paths)
	out["deadbeefunlisted"] = []string{"de/deadbeefunlisted"}
	return out, nil
}

// TestRestoreRejectsDBHashAbsentFromLists pins the coverage guard: a restored
// database that references a content hash appearing in no attachment list must
// fail the restore rather than silently skip materializing it. Without the
// guard the materialization loop, which iterates listed refs only, would never
// notice the extra reference and restore would report success over a database
// pointing at a missing content file.
func TestRestoreRejectsDBHashAbsentFromLists(t *testing.T) {
	require := require.New(t)
	ctx := context.Background()
	r := initTestRepo(t)
	dbPath, attachmentsDir, dataDir, _ := seedBackupFixture(t)
	cacheDir := t.TempDir()

	_, err := Create(ctx, r, newTestApp(), createOpts(dbPath, attachmentsDir, dataDir, cacheDir))
	require.NoError(err)

	_, err = Restore(ctx, r, extraContentPathApp{App: newTestApp()}, RestoreOptions{
		TargetDir: filepath.Join(t.TempDir(), "restore"),
	})
	require.ErrorContains(err, "appears in no attachment list")
	require.ErrorContains(err, "deadbeefunlisted")
}
