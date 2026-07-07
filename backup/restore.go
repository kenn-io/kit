package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"go.kenn.io/kit/pack"
)

// RestoreOptions parameterizes one restore run (FORMAT.md, Restore).
type RestoreOptions struct {
	SnapshotID string // empty: latest
	// TargetDir receives the restored archive: the database file, content dir,
	// and any captured extras. It must not exist, or be an empty directory,
	// unless Overwrite is set. Overwrite merges into the existing tree: the
	// database and its SQLite sidecars are removed first (a stale -wal, -shm,
	// or -journal would otherwise be replayed over the restored file on its
	// first normal open), restored files replace same-named ones, and files
	// the snapshot does not carry are left in place.
	TargetDir string
	Overwrite bool
	// Jobs is the number of concurrent pack-read workers. Zero or negative
	// selects one per CPU. Use 1 to read packs strictly one at a time when
	// the repository lives on a spinning disk or NAS share.
	Jobs        int
	ForceUnlock bool
	// Progress, if non-nil, receives structured progress events as Restore
	// runs. nil means fully silent.
	Progress func(ProgressEvent)
}

// RestoreResult reports what Restore materialized and proved.
type RestoreResult struct {
	SnapshotID      string
	DBPath          string
	DBBytes         int64
	AttachmentBlobs int64
	AttachmentBytes int64
	ExtrasFiles     int
	Duration        time.Duration
}

// Restore materializes one snapshot into TargetDir and then proves the
// result (FORMAT.md, Restore): every database page
// is hash-verified against the snapshot's page-hash map as it is written,
// every blob read re-derives its SHA-256 identity, and the restored database
// must pass PRAGMA integrity_check and reproduce the manifest's recorded
// stats exactly before Restore reports success.
//
// It takes a SHARED repository lock: concurrent restores and verifies are
// safe, a running create is not.
func Restore(ctx context.Context, r *Repo, app App, opts RestoreOptions) (*RestoreResult, error) {
	start := time.Now()
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	if opts.TargetDir == "" {
		return nil, errors.New("backup: restore target directory is required")
	}
	// Normalize the target once, before anything resolves it: with a trailing
	// separator ("link/", "link/.") POSIX resolves a final-component symlink
	// during lstat, so openRestoreRoot's leaf check — and verifyHeldTarget's
	// re-checks — would pass on a symlinked target addressed that way and
	// redirect every restore write into the link's destination. Cleaning here
	// also keeps every downstream consumer of the path (sync ceiling, DBPath,
	// the held-target re-verification) on one canonical spelling.
	opts.TargetDir = filepath.Clean(opts.TargetDir)
	lock, err := r.AcquireSharedLock("restore", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()

	var m *Manifest
	if opts.SnapshotID != "" {
		if m, err = r.LoadManifest(opts.SnapshotID); err != nil {
			return nil, err
		}
	} else {
		if m, err = r.LatestSnapshot(); err != nil {
			return nil, err
		}
		if m == nil {
			return nil, errors.New("backup: repository has no snapshots to restore")
		}
	}
	// The ceiling must be observed BEFORE prepareRestoreTarget creates the
	// target: it marks the deepest directory that already existed, so the
	// final durability pass knows which ancestors gained new entries.
	syncCeiling := restoreSyncCeiling(opts.TargetDir)
	root, err := prepareRestoreTarget(opts.TargetDir, opts.Overwrite, app.DBFileName())
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	jobs := opts.Jobs
	if jobs <= 0 {
		jobs = runtime.GOMAXPROCS(0)
	}
	st := &restoreState{
		repo:     r,
		app:      app,
		known:    known,
		jobs:     jobs,
		progress: newProgressEmitter(opts.Progress),
		root:     root,
		target:   opts.TargetDir,
	}

	hm, pm, err := st.materializeMaps(m)
	if err != nil {
		return nil, err
	}
	res := &RestoreResult{
		SnapshotID: m.SnapshotID,
		DBPath:     filepath.Join(opts.TargetDir, app.DBFileName()),
		DBBytes:    int64(pm.PageCount * uint64(pm.PageSize)), //nolint:gosec // geometry checked against the manifest
	}
	if err := st.restoreDB(ctx, app.DBFileName(), pm, hm); err != nil {
		return nil, err
	}
	res.AttachmentBlobs, res.AttachmentBytes, err = st.restoreAttachments(
		ctx, app, m, app.ContentDirName())
	if err != nil {
		return nil, err
	}
	if res.ExtrasFiles, err = st.restoreExtras(app, m); err != nil {
		return nil, err
	}

	// The proof has two visible steps: integrity_check (which reads the
	// whole restored database inside SQLite and dominates on large
	// archives) and the manifest stats comparison.
	st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Total: 2})
	if err := st.proveRestoredDB(ctx, m, func() {
		st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Done: 1, Total: 2})
	}); err != nil {
		return nil, err
	}
	st.progress.emit(ProgressEvent{Stage: ProgressStageProof, Done: 2, Total: 2, Final: true})

	if err := syncRestoredTree(opts.TargetDir, syncCeiling); err != nil {
		return nil, err
	}
	res.Duration = time.Since(start)
	return res, nil
}

// restoreSyncCeiling returns the deepest ancestor of target that already
// exists — or target itself when it does. Every directory restore creates
// below the ceiling (the target and any missing ancestors os.MkdirAll fills
// in) adds a directory entry that syncRestoredTree must make durable, and
// the ceiling itself receives the topmost new entry.
func restoreSyncCeiling(target string) string {
	p := target
	for {
		if _, err := os.Stat(p); err == nil {
			return p
		}
		parent := filepath.Dir(p)
		if parent == p {
			return p
		}
		p = parent
	}
}

// syncRestoredTree fsyncs every directory under target, deepest first, then
// upward from target's parent through ceiling, so the directory ENTRIES of
// everything restore created — nested attachment fan-out directories, extras
// subtrees, the target itself and any ancestors created for it — are as
// durable as the file contents by the time Restore reports success.
// writeRestoredFile fsyncs each file's bytes but not the directories naming
// them; without this pass a crash shortly after a successful restore could
// lose newly created paths. One sync per directory at the end costs far less
// than fsyncing parents on every file write, and the guarantee only needs to
// hold at success.
func syncRestoredTree(target, ceiling string) error {
	var dirs []string
	err := filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("backup: walking restore target for directory sync: %w", err)
	}
	// The walk visits parents before their children; going backward yields
	// every directory after all its descendants, so each entry is durable
	// in its parent by the time that parent is synced.
	for _, dir := range slices.Backward(dirs) {
		if err := pack.SyncDir(dir); err != nil {
			return fmt.Errorf("backup: syncing restored directory: %w", err)
		}
	}
	// Ancestors restore created above target, and the pre-existing ceiling
	// directory that received the topmost new entry, sit outside the walk;
	// climbing from target's parent continues the deepest-first order. When
	// the ceiling is the target itself, the target predates this restore
	// and nothing above it changed.
	if ceiling == target {
		return nil
	}
	for p := filepath.Dir(target); ; p = filepath.Dir(p) {
		if err := pack.SyncDir(p); err != nil {
			return fmt.Errorf("backup: syncing restored directory: %w", err)
		}
		if p == ceiling || p == filepath.Dir(p) {
			return nil
		}
	}
}

// sqliteSidecarNames lists the sidecar files SQLite may create next to a
// database: the WAL and its shared-memory index, and the rollback journal. A
// stale sidecar next to a restored database would be replayed or reused on
// the file's first normal open, silently altering the proven bytes — so
// overwrite restores remove them and extras entries may never plant one.
func sqliteSidecarNames(dbFileName string) []string {
	return []string{dbFileName + "-wal", dbFileName + "-shm", dbFileName + "-journal"}
}

// prepareRestoreTarget creates TargetDir, refusing a non-empty existing
// directory unless overwrite is set (FORMAT.md, Restore), and returns a root
// confined to it. Every subsequent restore write goes through that root, which
// refuses symlink escapes at the OS level, so a preexisting or raced symlink
// in the tree cannot redirect a write outside TargetDir.
func prepareRestoreTarget(target string, overwrite bool, dbFileName string) (*os.Root, error) {
	if target == "" {
		return nil, errors.New("backup: restore target directory is required")
	}
	entries, err := os.ReadDir(target)
	existed := true
	switch {
	case errors.Is(err, os.ErrNotExist):
		existed = false
		if err := os.MkdirAll(target, 0o700); err != nil {
			return nil, fmt.Errorf("backup: creating restore target: %w", err)
		}
	case err != nil:
		return nil, fmt.Errorf("backup: reading restore target: %w", err)
	case len(entries) > 0 && !overwrite:
		return nil, fmt.Errorf("backup: restore target %s is not empty (use --overwrite to restore into it anyway)", target)
	}
	root, err := openRestoreRoot(target)
	if err != nil {
		return nil, err
	}
	if !existed {
		return root, nil
	}
	// Overwrite merges rather than clearing the tree, but the database and
	// its SQLite sidecars must not survive: restoreDB rewrites the database
	// file, and a stale -wal/-shm pair or hot -journal next to it would be
	// replayed over the proven bytes on the file's first normal
	// (non-immutable) open. Remove them through the root so a symlink at any
	// of those names is unlinked, never followed.
	for _, name := range append([]string{dbFileName}, sqliteSidecarNames(dbFileName)...) {
		if err := root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = root.Close()
			return nil, fmt.Errorf("backup: removing stale %s from restore target: %w", name, err)
		}
	}
	// A restore killed mid-run leaves the database's staging temp
	// (<db>.restore-<ulid>, restoreDB) at the target root; a later overwrite
	// rerun would otherwise ignore it forever. Sweep only that exact
	// top-level shape — never a broad glob.
	if err := removeRestoreTempFiles(root, dbFileName); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

// removeRestoreTempFiles deletes the database staging temp files a crashed
// restore may have left at the target's top level. The name shape is exactly
// restoreDB's: dbFileName + ".restore-" + a 26-char lowercase ULID. The sweep
// runs entirely through the already-verified root — never a fresh pathname
// resolution of target — so a symlink swapped in at target after openRestoreRoot
// proved it cannot redirect the list or the removals outside the restore tree.
func removeRestoreTempFiles(root *os.Root, dbFileName string) error {
	prefix := dbFileName + ".restore-"
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("backup: scanning restore target for stale temp files: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		id, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !pack.IsValidPackID(id) {
			continue
		}
		if err := root.Remove(e.Name()); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: removing stale restore temp %s: %w", e.Name(), err)
		}
	}
	return nil
}

// openRestoreRoot opens a root confined to the restore target directory and
// proves the descriptor it holds is the real (non-symlink) directory at
// target. os.OpenRoot follows a final-component symlink, so a link planted at
// the target — before this run, or raced in against prepareRestoreTarget's
// MkdirAll — would otherwise redirect the entire restore under its link
// target. Checking AFTER the open closes the check-then-open race: Lstat
// reports a swapped-in link itself, and SameFile ties the lstat'd directory
// to the descriptor the root actually holds. Ancestor symlinks stay allowed
// (the path is user-supplied and resolved once); only the leaf is verified.
func openRestoreRoot(target string) (*os.Root, error) {
	root, err := os.OpenRoot(target)
	if err != nil {
		return nil, fmt.Errorf("backup: opening restore target: %w", err)
	}
	leaf, err := os.Lstat(target)
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("backup: checking restore target: %w", err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		_ = root.Close()
		return nil, fmt.Errorf(
			"backup: restore target %s is a symlink; pass the real directory path", target)
	}
	held, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("backup: checking restore target: %w", err)
	}
	if !os.SameFile(leaf, held) {
		_ = root.Close()
		return nil, fmt.Errorf(
			"backup: restore target %s was replaced while opening it", target)
	}
	return root, nil
}

// restoreState carries the shared read machinery for one Restore run. mu
// guards progress counters and the first-error slot while pack workers run.
type restoreState struct {
	repo     *Repo
	app      App
	known    map[pack.BlobID]IndexEntry
	jobs     int
	progress *progressEmitter
	// root confines every restore write beneath the verified target directory;
	// its methods refuse any path that escapes via symlink. target is the
	// caller-supplied path root was opened at; SQLite opens must go by path,
	// so verifyHeldTarget re-ties that path to root around each one.
	root   *os.Root
	target string

	mu       sync.Mutex
	firstErr error
	done     int64 // stage-local progress counter (pages, then files)
	doneByte int64
}

// fail records the run's first error; workers check failed() to stop early.
func (s *restoreState) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.firstErr == nil {
		s.firstErr = err
	}
}

func (s *restoreState) failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr != nil
}

// fetch reads one metadata blob through the repository index.
func (s *restoreState) fetch(id pack.BlobID) ([]byte, error) {
	return s.repo.ReadBlob(s.known, id, nil, s.app.PackFileExtension())
}

// materializeMaps walks and materializes the snapshot's hash-map and
// page-map chains, cross-checking both against the manifest's recorded
// geometry and the page map's coverage before any byte is written.
func (s *restoreState) materializeMaps(m *Manifest) (*PageHashMap, *PageMap, error) {
	hashChain, err := s.repo.HashMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	hm, err := MaterializeHashMap(s.fetch, hashChain)
	if err != nil {
		return nil, nil, err
	}
	pageChain, err := s.repo.PageMapChain(m)
	if err != nil {
		return nil, nil, err
	}
	pm, err := MaterializePageMap(s.fetch, pageChain)
	if err != nil {
		return nil, nil, err
	}
	if err := pm.CheckCoverage(); err != nil {
		return nil, nil, err
	}
	if pm.PageCount != m.DB.PageCount || pm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			pm.PageCount, pm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	if hm.PageCount != m.DB.PageCount || hm.PageSize != m.DB.PageSize {
		return nil, nil, fmt.Errorf(
			"backup: page hash map geometry (%d pages of %d bytes) disagrees with manifest (%d pages of %d bytes)",
			hm.PageCount, hm.PageSize, m.DB.PageCount, m.DB.PageSize)
	}
	return hm, pm, nil
}

// blobRuns is one page blob and every page-map run it backs.
type blobRuns struct {
	id   pack.BlobID
	runs []PageRun
}

// restoreDB materializes the database file: every page-map run is written at
// page*page_size, and every page is hash-verified against the page-hash map
// while its blob is still in memory. Work is grouped by pack, s.jobs packs
// in flight; the file writes are disjoint pwrite calls, safe concurrently.
func (s *restoreState) restoreDB(ctx context.Context, dbRel string, pm *PageMap, hm *PageHashMap) error {
	// Write into a fresh, unpredictably named temp inside the root, then rename
	// it over dbRel: a preexisting symlink at dbRel is replaced rather than
	// followed, and the concurrent pwrite workers below never touch dbRel
	// itself until the proven bytes are complete.
	tmpRel := dbRel + ".restore-" + pack.NewPackID()
	f, err := s.root.OpenFile(tmpRel, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("backup: creating restored database: %w", err)
	}
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = s.root.Remove(tmpRel)
		}
	}()
	size := int64(pm.PageCount * uint64(pm.PageSize)) //nolint:gosec // geometry checked against the manifest
	if err := f.Truncate(size); err != nil {
		return fmt.Errorf("backup: sizing restored database: %w", err)
	}

	runsByBlob := make(map[uint32][]PageRun)
	for _, r := range pm.Runs {
		runsByBlob[r.BlobIndex] = append(runsByBlob[r.BlobIndex], r)
	}
	groups := map[string][]blobRuns{}
	var order []string
	for i, id := range pm.Blobs {
		ie, ok := s.known[id]
		if !ok {
			return fmt.Errorf("backup: page blob %s not present in any index", id)
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], blobRuns{id: id, runs: runsByBlob[uint32(i)]})
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Total: int64(pm.PageCount), BytesTotal: size, //nolint:gosec // page counts fit int64
	})
	err = s.runPackGroups(ctx, order, func(packID string) {
		s.restorePackPages(f, packID, groups[packID], pm.PageSize, hm)
	})
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("backup: syncing restored database: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: closing restored database: %w", err)
	}
	if err := s.root.Rename(tmpRel, dbRel); err != nil {
		return fmt.Errorf("backup: publishing restored database: %w", err)
	}
	committed = true
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: int64(pm.PageCount), Total: int64(pm.PageCount), //nolint:gosec // page counts fit int64
		BytesDone: size, BytesTotal: size, Final: true,
	})
	return nil
}

// runPackGroups fans packIDs out to s.jobs workers, stopping dispatch on
// context cancellation or the first recorded failure, and returns the run's
// first error.
func (s *restoreState) runPackGroups(ctx context.Context, packIDs []string, work func(packID string)) error {
	packs := make(chan string)
	var wg sync.WaitGroup
	for range max(min(s.jobs, len(packIDs)), 1) {
		wg.Go(func() {
			for packID := range packs {
				if s.failed() {
					continue
				}
				work(packID)
			}
		})
	}
	for _, packID := range packIDs {
		if err := ctx.Err(); err != nil {
			s.fail(err)
			break
		}
		if s.failed() {
			break
		}
		packs <- packID
	}
	close(packs)
	wg.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstErr
}

// restorePackPages writes one pack's page blobs into the database file.
func (s *restoreState) restorePackPages(f *os.File, packID string, blobs []blobRuns, pageSize uint32, hm *PageHashMap) {
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, br := range blobs {
		if s.failed() {
			return
		}
		entry, ok := entryByID[br.id]
		if !ok {
			s.fail(fmt.Errorf("backup: page blob %s missing from pack %s footer", br.id, packID))
			return
		}
		raw, err := pr.ReadBlob(*entry)
		if err != nil {
			s.fail(fmt.Errorf("backup: reading page blob %s from pack %s: %w", br.id, packID, err))
			return
		}
		for _, run := range br.runs {
			if err := s.writeRun(f, raw, br.id, run, pageSize, hm); err != nil {
				s.fail(err)
				return
			}
		}
	}
}

// writeRun hash-verifies and writes one page-map run from its blob's bytes.
func (s *restoreState) writeRun(f *os.File, raw []byte, id pack.BlobID, run PageRun, pageSize uint32, hm *PageHashMap) error {
	length := uint64(run.PageCount) * uint64(pageSize)
	// Subtraction-based bounds check: BlobOffset is untrusted (decoded from
	// a page-map object), and BlobOffset+length can wrap uint64 for a huge
	// offset, slipping past an addition-based comparison into a slice panic.
	if run.BlobOffset > uint64(len(raw)) || length > uint64(len(raw))-run.BlobOffset {
		return fmt.Errorf(
			"backup: page map run (pages %d..%d) overruns blob %s (%d bytes at offset %d)",
			run.StartPage, run.StartPage+uint64(run.PageCount)-1, id, len(raw), run.BlobOffset)
	}
	segment := raw[run.BlobOffset : run.BlobOffset+length]
	for i := range uint64(run.PageCount) {
		p := run.StartPage + i
		h := PageHash(segment[i*uint64(pageSize) : (i+1)*uint64(pageSize)])
		if !bytes.Equal(h[:], hm.Hashes[p*pageHashSize:(p+1)*pageHashSize]) {
			return fmt.Errorf("backup: restored page %d does not match the snapshot's page hash map", p)
		}
	}
	if _, err := f.WriteAt(segment, int64(run.StartPage)*int64(pageSize)); err != nil { //nolint:gosec // page*pageSize fits int64
		return fmt.Errorf("backup: writing pages %d..%d: %w", run.StartPage, run.StartPage+uint64(run.PageCount)-1, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.done += int64(run.PageCount)
	s.doneByte += int64(length) //nolint:gosec // run lengths fit int64
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageRestoreDB, Done: s.done, Total: int64(hm.PageCount), //nolint:gosec // page counts fit int64
		BytesDone: s.doneByte, BytesTotal: int64(hm.PageCount) * int64(pageSize), //nolint:gosec // page counts fit int64
	})
	return nil
}

// restoreAttachments lays the snapshot's attachment population out at the
// storage paths the restored database records for each hash (importers may
// namespace paths beyond the plain <hash[:2]>/<hash> layout), reading blobs
// grouped by pack. Every blob read re-derives its SHA-256 identity before
// any file is written.
func (s *restoreState) restoreAttachments(ctx context.Context, app App, m *Manifest, contentDir string) (int64, int64, error) {
	refs, _, err := LoadListRefs(s.repo, s.known, m.Attachments.Lists, nil, app.PackFileExtension())
	if err != nil {
		return 0, 0, err
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		return 0, 0, fmt.Errorf(
			"backup: attachment lists name %d blobs but manifest reports %d", len(refs), m.Attachments.Blobs)
	}
	paths, err := s.restoredContentPaths(ctx)
	if err != nil {
		return 0, 0, err
	}
	// The materialization loop below iterates listed refs only, so a hash the
	// restored database references but no attachment list carries would never
	// be written, yet restore would still report success. Prove full coverage
	// of the DB's content references up front: every path key must trace back
	// to a listed blob. The complementary direction (listed hash with no DB
	// path) is caught per-blob in restorePackAttachments.
	listed := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		listed[ref.Hash] = struct{}{}
	}
	for hash := range paths {
		if _, ok := listed[hash]; !ok {
			return 0, 0, fmt.Errorf(
				"backup: restored database references attachment %s that appears in no attachment list", hash)
		}
	}
	// Two different content hashes must never claim the same restore path: the
	// attachment writer publishes each blob with a temp-then-rename that
	// clobbers, so a shared path would silently leave whichever blob finished
	// last and discard the other while restore still reported success. Reject up
	// front. The same path repeated under one hash is harmless (identical
	// writes), so key the check on the cleaned path and only fail on a
	// conflicting owner.
	//
	// The collision key is deliberately conservative: it case-folds the cleaned
	// path (the same folding restoreExtrasEntry's reserved-name guard uses)
	// because case-insensitive filesystems (default macOS, Windows) alias "A/b"
	// and "a/b" onto one file, and it rejects any component ending in a dot or
	// space (which Windows trims). This also makes restore stricter on
	// case-sensitive filesystems — two paths differing only by case now collide
	// — which is the intended contract: a snapshot that only restores correctly
	// on a case-sensitive host is not safe to call restorable.
	pathOwner := make(map[string]string)
	for hash, rels := range paths {
		for _, rel := range rels {
			// A non-local path is a more fundamental error than a collision and
			// has no meaningful normalized key; reject it first (the per-write
			// path below re-checks as defense in depth).
			if !filepath.IsLocal(rel) {
				return 0, 0, fmt.Errorf(
					"backup: attachment %s restore path %q escapes the content directory", hash, rel)
			}
			if bad := trailingDotOrSpaceComponent(rel); bad != "" {
				return 0, 0, fmt.Errorf(
					"backup: attachment %s restore path %q has component %q ending in a dot or space", hash, rel, bad)
			}
			key := foldedPathKey(rel)
			if other, ok := pathOwner[key]; ok && other != hash {
				return 0, 0, fmt.Errorf(
					"backup: restore path %q collides under case-folded key %q with two different attachments %s and %s",
					rel, key, other, hash)
			}
			pathOwner[key] = hash
		}
	}
	groups := map[string][]ContentRef{}
	var order []string
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		if err != nil {
			return 0, 0, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
		}
		ie, ok := s.known[id]
		if !ok {
			return 0, 0, fmt.Errorf("backup: attachment blob %s not present in any index", ref.Hash)
		}
		if _, seen := groups[ie.PackID]; !seen {
			order = append(order, ie.PackID)
		}
		groups[ie.PackID] = append(groups[ie.PackID], ref)
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Total: int64(len(refs)), BytesTotal: m.Attachments.BlobBytes,
	})
	err = s.runPackGroups(ctx, order, func(packID string) {
		s.restorePackAttachments(contentDir, packID, groups[packID], paths, int64(len(refs)), m.Attachments.BlobBytes)
	})
	if err != nil {
		return 0, 0, err
	}
	var totalBytes int64
	for _, ref := range refs {
		totalBytes += ref.Size
	}
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: int64(len(refs)), Total: int64(len(refs)),
		BytesDone: totalBytes, BytesTotal: totalBytes, Final: true,
	})
	return int64(len(refs)), totalBytes, nil
}

// sqliteURIDSN builds a file: SQLite URI for path carrying rawQuery as its
// connection parameters. Built via url.URL so a path containing '?' or '#'
// cannot be misparsed as URI syntax — a naive path+"?params" concatenation
// would open a different (usually freshly created, empty) file when the
// path itself contains '?'. The path is made absolute first — a relative
// path must not become slash-rooted — then converted to the
// slash-separated, slash-rooted form SQLite's URI parser requires
// ("file:///C:/dir/app.db"): a raw drive-letter path would otherwise
// be read as a URI authority.
func sqliteURIDSN(path, rawQuery string) string {
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	p := filepath.ToSlash(path)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p, RawQuery: rawQuery}).String()
}

// restoredDBDSN builds the immutable read-only SQLite URI for the restored
// database. immutable=1 matters: the file has no writers and no WAL sidecar,
// and an immutable open never creates -wal/-shm next to it.
func restoredDBDSN(dbPath string) string {
	return sqliteURIDSN(dbPath, "immutable=1&mode=ro")
}

// verifyHeldTarget re-proves that the caller-supplied target path still
// resolves to the directory the held root descriptor pins, and that the
// database file inside it is the same file the root sees. openRestoreRoot
// proved this once; SQLite opens re-resolve the path, so an actor who can
// rename the target in its parent could otherwise point them at a different
// database than the one restore wrote and hash-verified through the root.
func (s *restoreState) verifyHeldTarget(dbRel string) error {
	leaf, err := os.Lstat(s.target)
	if err != nil {
		return fmt.Errorf("backup: re-checking restore target: %w", err)
	}
	if leaf.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"backup: restore target %s was replaced with a symlink during restore", s.target)
	}
	held, err := s.root.Stat(".")
	if err != nil {
		return fmt.Errorf("backup: re-checking restore target: %w", err)
	}
	if !os.SameFile(leaf, held) {
		return fmt.Errorf(
			"backup: restore target %s was replaced during restore", s.target)
	}
	byPath, err := os.Stat(filepath.Join(s.target, dbRel))
	if err != nil {
		return fmt.Errorf("backup: re-checking restored database: %w", err)
	}
	byRoot, err := s.root.Stat(dbRel)
	if err != nil {
		return fmt.Errorf("backup: re-checking restored database: %w", err)
	}
	if !os.SameFile(byPath, byRoot) {
		return fmt.Errorf(
			"backup: restored database %s was replaced during restore",
			filepath.Join(s.target, dbRel))
	}
	return nil
}

// openRestoredDB opens the restored database read-only for the proof queries.
// SQLite can only open by path, which re-resolves the target directory, so
// after forcing the first connection open this re-verifies that the path
// still leads to the directory and database file the held root pins, and the
// callers verify once more after their queries complete. A replacement that
// persists at either check fails the restore; only a replace-and-restore
// race timed exactly around a connection's own open remains outside what a
// path-based open can detect.
func (s *restoreState) openRestoredDB(ctx context.Context) (*sql.DB, error) {
	dbRel := s.app.DBFileName()
	db, err := sql.Open("sqlite3", restoredDBDSN(filepath.Join(s.target, dbRel)))
	if err != nil {
		return nil, fmt.Errorf("backup: opening restored database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("backup: opening restored database: %w", err)
	}
	if err := s.verifyHeldTarget(dbRel); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// restoredContentPaths opens the restored database read-only and asks the app
// to re-derive hash → relative content paths from it, so restore can
// materialize and verify every referenced file at the paths the app records.
func (s *restoreState) restoredContentPaths(ctx context.Context) (map[string][]string, error) {
	db, err := s.openRestoredDB(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	paths, err := s.app.RestoredContentPaths(ctx, db)
	if err != nil {
		return nil, err
	}
	// The derived paths decide where attachment bytes land inside the root;
	// fail closed if the database they came from is no longer the one the
	// root holds.
	if err := s.verifyHeldTarget(s.app.DBFileName()); err != nil {
		return nil, err
	}
	return paths, nil
}

// restorePackAttachments writes one pack's attachment blobs to their
// recorded storage paths under dir.
func (s *restoreState) restorePackAttachments(
	contentDir, packID string, refs []ContentRef, paths map[string][]string, total, totalBytes int64,
) {
	pr, err := pack.OpenReader(s.repo.packPath(packID, s.app.PackFileExtension()), nil)
	if err != nil {
		s.fail(fmt.Errorf("backup: opening pack %s: %w", packID, err))
		return
	}
	defer func() { _ = pr.Close() }()
	entries := pr.Entries()
	entryByID := make(map[pack.BlobID]*pack.Entry, len(entries))
	for i := range entries {
		entryByID[entries[i].ID] = &entries[i]
	}
	for _, ref := range refs {
		if s.failed() {
			return
		}
		id, _ := pack.ParseBlobID(ref.Hash) // validated during grouping
		entry, ok := entryByID[id]
		if !ok {
			s.fail(fmt.Errorf("backup: attachment blob %s missing from pack %s footer", ref.Hash, packID))
			return
		}
		content, err := pr.ReadBlob(*entry)
		if err != nil {
			s.fail(fmt.Errorf("backup: reading attachment %s from pack %s: %w", ref.Hash, packID, err))
			return
		}
		if int64(len(content)) != ref.Size {
			s.fail(fmt.Errorf(
				"backup: attachment %s is %d bytes but its list records %d", ref.Hash, len(content), ref.Size))
			return
		}
		rels := paths[ref.Hash]
		if len(rels) == 0 {
			// The manifest's stats proof pins list count == DB blob count, so
			// a listed hash with no DB path means the two sets diverge.
			s.fail(fmt.Errorf(
				"backup: attachment blob %s is in the snapshot's lists but the restored database records no path for it",
				ref.Hash))
			return
		}
		for _, rel := range rels {
			// rel comes from the restored database via App.RestoredContentPaths,
			// so it is re-validated here before being joined into dir — the same
			// untrusted provenance restoreExtrasEntry guards against for extras
			// tree paths.
			if !filepath.IsLocal(rel) {
				s.fail(fmt.Errorf(
					"backup: attachment %s restore path %q escapes the content directory", ref.Hash, rel))
				return
			}
			if err := s.writeRootFile(filepath.Join(contentDir, rel), content, 0o600); err != nil {
				s.fail(err)
				return
			}
		}
		s.mu.Lock()
		s.done++
		s.doneByte += ref.Size
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageAttachments, Done: s.done, Total: total,
			BytesDone: s.doneByte, BytesTotal: totalBytes,
		})
		s.mu.Unlock()
	}
}

// restoreExtras lays out the snapshot's captured extras files (the
// application-selected operational files, ExtrasSpec) under the target,
// preserving their recorded modes.
func (s *restoreState) restoreExtras(app App, m *Manifest) (int, error) {
	if m.Extras.Tree == "" {
		return 0, nil
	}
	id, err := pack.ParseBlobID(m.Extras.Tree)
	if err != nil {
		return 0, fmt.Errorf("backup: extras tree blob id %q: %w", m.Extras.Tree, err)
	}
	raw, err := s.fetch(id)
	if err != nil {
		return 0, err
	}
	var tree ExtrasTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return 0, fmt.Errorf("backup: extras tree %s: %w", id, err)
	}
	if err := checkExtrasCollisions(tree.Entries); err != nil {
		return 0, err
	}
	s.progress.emit(ProgressEvent{Stage: ProgressStageExtras, Total: int64(len(tree.Entries))})
	for i, entry := range tree.Entries {
		if err := s.restoreExtrasEntry(app, entry); err != nil {
			return 0, err
		}
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageExtras, Done: int64(i + 1), Total: int64(len(tree.Entries)),
			Final: i == len(tree.Entries)-1,
		})
	}
	return len(tree.Entries), nil
}

// foldedPathKey is the collision key restore uses to detect distinct archive
// paths that alias one file on the restoring host: case-insensitive
// filesystems (default macOS, Windows) fold "A/b" and "a/b" onto one file, so
// the key case-folds the cleaned OS form of the path. Both the attachment and
// extras collision checks must use this same key.
func foldedPathKey(rel string) string {
	return strings.ToLower(filepath.Clean(filepath.FromSlash(rel)))
}

// checkExtrasCollisions rejects an extras tree in which two entries resolve
// to one file. writeRootFile publishes each entry with a clobbering rename,
// so a duplicate path — or two paths differing only by case, which alias one
// file on case-insensitive filesystems — would silently keep whichever entry
// restored last while restore still reported every entry as written. As with
// the attachment check, this deliberately also rejects such trees on
// case-sensitive hosts: a snapshot that only restores correctly on a
// case-sensitive filesystem is not safe to call restorable.
func checkExtrasCollisions(entries []ExtrasEntry) error {
	owner := make(map[string]string, len(entries))
	for _, entry := range entries {
		key := foldedPathKey(entry.Path)
		other, ok := owner[key]
		switch {
		case !ok:
			owner[key] = entry.Path
		case other == entry.Path:
			return fmt.Errorf("backup: extras tree lists path %q twice", entry.Path)
		default:
			return fmt.Errorf(
				"backup: extras entries %q and %q collide under case-folded key %q",
				other, entry.Path, key)
		}
	}
	return nil
}

// trailingDotOrSpaceComponent returns the first component of rel (a relative
// path in slash or OS-separator form) that ends in a dot or space, or "" when
// none does. Windows resolves such a component to its trimmed name, so two
// distinct-looking archive paths can alias the same file; restore rejects them
// on every platform to keep its reserved-name and path-collision guards sound
// regardless of the restoring host's OS.
func trailingDotOrSpaceComponent(rel string) string {
	for comp := range strings.SplitSeq(filepath.ToSlash(rel), "/") {
		if strings.TrimRight(comp, ". ") != comp {
			return comp
		}
	}
	return ""
}

// validateExtrasEntryPath checks one extras entry path against the rules
// restore enforces before writing, returning the cleaned OS-form path the
// entry restores to. Entry paths come from a decoded tree blob, so they are
// re-validated: only local, relative, traversal-free paths may be written
// under the target, and never paths that overlap the database or attachments
// restore already produced. Verify runs the same checks so a snapshot that
// restore would refuse cannot verify cleanly.
//
// The path is cleaned before validating: the final filepath.Join cleans it
// anyway, so validating the raw form would let "safe/../app.db" pass the
// reserved-name check yet still land on a reserved path. Components ending
// in a dot or space resolve to the trimmed name on Windows, so "content."
// would alias the reserved content dir past the folded comparison; they are
// rejected on every platform to keep the guard sound regardless of OS. The
// reserved names come from the app so a generic application's extras can
// never overwrite its restored DB or content tree; the comparison is folded
// because the default macOS filesystem is case-insensitive.
func validateExtrasEntryPath(path, contentDirName, dbFileName string) (string, error) {
	rel := filepath.Clean(filepath.FromSlash(path))
	if path == "" || rel == "." || filepath.IsAbs(rel) || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("backup: extras entry path %q escapes the restore target", path)
	}
	if trailingDotOrSpaceComponent(rel) != "" {
		return "", fmt.Errorf("backup: extras entry path %q has a component ending in a dot or space", path)
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	for _, reserved := range append(
		[]string{contentDirName, dbFileName}, sqliteSidecarNames(dbFileName)...) {
		if strings.EqualFold(first, reserved) {
			return "", fmt.Errorf("backup: extras entry path %q overlaps restored archive content", path)
		}
	}
	return rel, nil
}

// restoreExtrasEntry validates and writes one extras file. Capture never
// records archive content as an extra, so an entry naming the restored
// database, its SQLite sidecars, or the content tree can only come from a
// tampered tree blob trying to overwrite already-proven outputs
// (validateExtrasEntryPath).
func (s *restoreState) restoreExtrasEntry(app App, entry ExtrasEntry) error {
	rel, err := validateExtrasEntryPath(entry.Path, app.ContentDirName(), app.DBFileName())
	if err != nil {
		return err
	}
	id, err := pack.ParseBlobID(entry.Blob)
	if err != nil {
		return fmt.Errorf("backup: extras entry %s blob id %q: %w", entry.Path, entry.Blob, err)
	}
	content, err := s.fetch(id)
	if err != nil {
		return err
	}
	if int64(len(content)) != entry.Size {
		return fmt.Errorf("backup: extras entry %s is %d bytes but its tree records %d",
			entry.Path, len(content), entry.Size)
	}
	mode := os.FileMode(entry.Mode).Perm()
	if mode == 0 {
		mode = 0o600
	}
	return s.writeRootFile(rel, content, mode)
}

// enterRestoreDir opens one directory component beneath cur as its own root,
// creating it if missing and refusing symlinks: os.Root only refuses symlinks
// that escape the root, so without this check a preexisting in-root symlink
// (overwrite mode merges into an existing tree) would silently redirect
// restored writes elsewhere inside the target — e.g. "deletions -> content"
// would land extras files in the already-proven content tree. The component
// must lstat as a real directory and the opened descriptor must be that same
// inode, so a symlink present up front and one raced in before the open both
// fail closed. Mirrors the capture-side enterExtrasDir.
func enterRestoreDir(cur *os.Root, comp string) (*os.Root, error) {
	li, err := cur.Lstat(comp)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := cur.Mkdir(comp, 0o700); mkErr != nil && !errors.Is(mkErr, os.ErrExist) {
			return nil, mkErr
		}
		li, err = cur.Lstat(comp)
	}
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 || !li.IsDir() {
		return nil, fmt.Errorf("path component %q is not a real directory", comp)
	}
	sub, err := cur.OpenRoot(comp)
	if err != nil {
		return nil, err
	}
	held, err := sub.Stat(".")
	if err != nil {
		_ = sub.Close()
		return nil, err
	}
	if !os.SameFile(li, held) {
		_ = sub.Close()
		return nil, fmt.Errorf("path component %q changed during restore", comp)
	}
	return sub, nil
}

// writeRootFile writes one restored file durably beneath the confined root:
// parents are created and entered one verified component at a time
// (enterRestoreDir — no symlink is ever followed, even one resolving inside
// the root), then the bytes are written to a fresh, unpredictably named temp
// in the leaf's directory and renamed over the leaf name within that held
// directory descriptor. The temp-then-rename replaces a preexisting symlink
// at rel rather than following it. rel is relative to the root (the restore
// target).
func (s *restoreState) writeRootFile(rel string, content []byte, mode os.FileMode) error {
	dir := s.root
	defer func() {
		if dir != s.root {
			_ = dir.Close()
		}
	}()
	comps := strings.Split(filepath.ToSlash(filepath.Clean(rel)), "/")
	for _, comp := range comps[:len(comps)-1] {
		sub, err := enterRestoreDir(dir, comp)
		if err != nil {
			return fmt.Errorf("backup: creating restore directory for %s: %w", rel, err)
		}
		if dir != s.root {
			_ = dir.Close()
		}
		dir = sub
	}
	name := comps[len(comps)-1]
	tmp := ".restore-" + pack.NewPackID()
	f, err := dir.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("backup: creating restored file %s: %w", rel, err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = dir.Remove(tmp)
		}
	}()
	// O_CREATE's mode is filtered through the umask; restore must reproduce
	// the captured mode exactly.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: setting mode on restored file %s: %w", rel, err)
	}
	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: writing restored file %s: %w", rel, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("backup: syncing restored file %s: %w", rel, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("backup: closing restored file %s: %w", rel, err)
	}
	if err := dir.Rename(tmp, name); err != nil {
		return fmt.Errorf("backup: publishing restored file %s: %w", rel, err)
	}
	committed = true
	return nil
}

// proveRestoredDB is the restore proof (FORMAT.md,
// Restore): the restored database must pass PRAGMA integrity_check and
// reproduce the manifest's recorded stats through exactly the queries
// capture ran inside the freeze. Page-level identity was already proven
// against the page-hash map during materialization.
// checked is called after integrity_check passes, before the stats
// comparison, so callers can report sub-step progress.
func (s *restoreState) proveRestoredDB(ctx context.Context, m *Manifest, checked func()) error {
	db, err := s.openRestoredDB(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()

	rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return fmt.Errorf("backup: restored database integrity_check: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var findings []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return fmt.Errorf("backup: reading integrity_check result: %w", err)
		}
		findings = append(findings, line)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("backup: integrity_check rows: %w", err)
	}
	if len(findings) != 1 || findings[0] != "ok" {
		return fmt.Errorf("backup: restored database failed integrity_check: %v", findings)
	}
	if checked != nil {
		checked()
	}

	restoredStats, err := s.app.RestoredStats(ctx, db)
	if err != nil {
		return err
	}
	// The proof is only about the database the root holds; fail closed if the
	// path the queries ran against no longer resolves to it.
	if err := s.verifyHeldTarget(s.app.DBFileName()); err != nil {
		return err
	}
	if !bytes.Equal(compactJSON(restoredStats), compactJSON(m.Stats)) {
		return fmt.Errorf("backup: restored database stats %s do not match manifest stats %s",
			restoredStats, m.Stats)
	}
	return nil
}

// compactJSON normalizes RawMessage formatting: manifests on disk are
// indented, and a RawMessage captured from an indented document keeps that
// indentation, while freshly marshaled stats are compact.
func compactJSON(raw json.RawMessage) []byte {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return buf.Bytes()
}
