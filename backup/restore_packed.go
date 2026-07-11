package backup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

type restoreAttachmentInventory struct {
	refs   []ContentRef
	paths  map[string][]string
	groups map[string][]ContentRef
	order  []string
}

type packedRestoreResult struct {
	totalBlobs  int64
	totalBytes  int64
	packedBlobs int64
	looseBlobs  int64
	packs       int
	fallbacks   []packstore.ImportFallback
}

// loadRestoreAttachmentInventory validates the snapshot lists, restored DB
// membership, restore paths, and repository index as one shared preflight for
// both the legacy loose writer and the optional mixed writer.
func (s *restoreState) loadRestoreAttachmentInventory(
	ctx context.Context, app App, m *Manifest,
) (restoreAttachmentInventory, error) {
	refs, _, err := LoadListRefs(s.repo, s.known, m.Attachments.Lists, nil, app.PackFileExtension())
	if err != nil {
		return restoreAttachmentInventory{}, err
	}
	if int64(len(refs)) != m.Attachments.Blobs {
		return restoreAttachmentInventory{}, fmt.Errorf(
			"backup: attachment lists name %d blobs but manifest reports %d", len(refs), m.Attachments.Blobs)
	}
	paths, err := s.restoredContentPaths(ctx)
	if err != nil {
		return restoreAttachmentInventory{}, err
	}
	listed := make(map[string]struct{}, len(refs))
	for _, ref := range refs {
		listed[ref.Hash] = struct{}{}
		if len(paths[ref.Hash]) == 0 {
			return restoreAttachmentInventory{}, fmt.Errorf(
				"backup: attachment blob %s is in the snapshot's lists but the restored database records no path for it",
				ref.Hash)
		}
	}
	for hash := range paths {
		if _, ok := listed[hash]; !ok {
			return restoreAttachmentInventory{}, fmt.Errorf(
				"backup: restored database references attachment %s that appears in no attachment list", hash)
		}
	}
	pathOwner := make(map[string]string)
	for hash, rels := range paths {
		for _, rel := range rels {
			if !filepath.IsLocal(rel) {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: attachment %s restore path %q escapes the content directory", hash, rel)
			}
			if bad := trailingDotOrSpaceComponent(rel); bad != "" {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: attachment %s restore path %q has component %q ending in a dot or space", hash, rel, bad)
			}
			key := foldedPathKey(rel)
			if other, ok := pathOwner[key]; ok && other != hash {
				return restoreAttachmentInventory{}, fmt.Errorf(
					"backup: restore path %q collides under case-folded key %q with two different attachments %s and %s",
					rel, key, other, hash)
			}
			pathOwner[key] = hash
		}
	}
	groups := make(map[string][]ContentRef)
	var order []string
	for _, ref := range refs {
		id, err := pack.ParseBlobID(ref.Hash)
		if err != nil {
			return restoreAttachmentInventory{}, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
		}
		entry, ok := s.known[id]
		if !ok {
			return restoreAttachmentInventory{}, fmt.Errorf("backup: attachment blob %s not present in any index", ref.Hash)
		}
		if _, seen := groups[entry.PackID]; !seen {
			order = append(order, entry.PackID)
		}
		groups[entry.PackID] = append(groups[entry.PackID], ref)
	}
	return restoreAttachmentInventory{refs: refs, paths: paths, groups: groups, order: order}, nil
}

func (s *restoreState) restorePackedAttachments(
	ctx context.Context,
	app App,
	m *Manifest,
	contentDir string,
	target PackedContentTarget,
	createdAt time.Time,
) (packedRestoreResult, error) {
	limits := target.Limits()
	if limits.BlobBytes < 0 || limits.PackBytes <= 0 || limits.FooterBytes <= 0 || limits.PackEntries <= 0 {
		return packedRestoreResult{}, fmt.Errorf("backup: packed content target returned invalid limits")
	}
	inventory, err := s.loadRestoreAttachmentInventory(ctx, app, m)
	if err != nil {
		return packedRestoreResult{}, err
	}
	result := packedRestoreResult{totalBlobs: int64(len(inventory.refs))}
	seenHashes := make(map[string]struct{}, len(inventory.refs))
	for _, ref := range inventory.refs {
		if _, duplicate := seenHashes[ref.Hash]; duplicate {
			return packedRestoreResult{}, fmt.Errorf(
				"backup: packed restore attachment lists name content hash %s more than once", ref.Hash)
		}
		seenHashes[ref.Hash] = struct{}{}
		if ref.Size > 0 && result.totalBytes > math.MaxInt64-ref.Size {
			return packedRestoreResult{}, fmt.Errorf("backup: attachment byte total overflows")
		}
		result.totalBytes += ref.Size
	}
	if result.totalBytes != m.Attachments.BlobBytes {
		return packedRestoreResult{}, fmt.Errorf(
			"backup: attachment lists sum to %d bytes but manifest reports %d",
			result.totalBytes, m.Attachments.BlobBytes)
	}

	var candidates []packstore.ImportPack
	if app.PackFileExtension() == packstore.PackExt {
		candidates, err = s.importCandidates(inventory, app.PackFileExtension())
		if err != nil {
			return packedRestoreResult{}, err
		}
	}
	prepared, err := packstore.PrepareImport(ctx, s.root, filepath.ToSlash(contentDir), candidates, packstore.ImportOptions{
		Limits: limits, CreatedAt: createdAt.UTC(),
	})
	if err != nil {
		return packedRestoreResult{}, fmt.Errorf("backup: preparing packed attachment restore: %w", err)
	}
	stats := prepared.Stats()
	if app.PackFileExtension() != packstore.PackExt {
		for _, packID := range inventory.order {
			stats.Fallbacks = append(stats.Fallbacks, packstore.ImportFallback{
				PackID: packID, Reason: packstore.FallbackPackEncoding,
			})
		}
	}
	packedSet := make(map[string]struct{}, stats.PackedBlobs)
	for _, hash := range prepared.PackedHashes() {
		packedSet[hash.String()] = struct{}{}
	}
	if len(packedSet) != stats.PackedBlobs {
		return packedRestoreResult{}, fmt.Errorf("backup: packed import reported inconsistent selected hash count")
	}

	s.done, s.doneByte = 0, 0
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Total: result.totalBlobs, BytesTotal: result.totalBytes,
	})
	looseGroups := make(map[string][]ContentRef)
	var looseOrder []string
	for _, packID := range inventory.order {
		for _, ref := range inventory.groups[packID] {
			if _, packed := packedSet[ref.Hash]; packed {
				s.done++
				s.doneByte += ref.Size
				continue
			}
			if len(looseGroups[packID]) == 0 {
				looseOrder = append(looseOrder, packID)
			}
			looseGroups[packID] = append(looseGroups[packID], ref)
		}
	}
	if s.done > 0 {
		s.progress.emit(ProgressEvent{
			Stage: ProgressStageAttachments, Done: s.done, Total: result.totalBlobs,
			BytesDone: s.doneByte, BytesTotal: result.totalBytes,
		})
	}
	if err := s.runPackGroups(ctx, looseOrder, func(packID string) {
		s.restorePackAttachments(contentDir, packID, looseGroups[packID], inventory.paths, result.totalBlobs, result.totalBytes)
	}); err != nil {
		return packedRestoreResult{}, err
	}
	result.packedBlobs = int64(len(packedSet))
	result.looseBlobs = result.totalBlobs - result.packedBlobs
	if result.packedBlobs < 0 || result.looseBlobs < 0 || result.packedBlobs+result.looseBlobs != result.totalBlobs {
		return packedRestoreResult{}, fmt.Errorf("backup: packed and loose attachment coverage is inconsistent")
	}
	if err := s.commitPreparedImport(ctx, target, prepared); err != nil {
		return packedRestoreResult{}, err
	}
	result.packs = stats.PackedPacks
	result.fallbacks = append([]packstore.ImportFallback(nil), stats.Fallbacks...)
	s.progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: result.totalBlobs, Total: result.totalBlobs,
		BytesDone: result.totalBytes, BytesTotal: result.totalBytes, Final: true,
	})
	return result, nil
}

func (s *restoreState) importCandidates(
	inventory restoreAttachmentInventory, ext string,
) ([]packstore.ImportPack, error) {
	candidates := make([]packstore.ImportPack, 0, len(inventory.order))
	for _, packID := range inventory.order {
		candidate := packstore.ImportPack{
			PackID: packID, SourcePath: s.repo.packPath(packID, ext),
			Selections: make([]packstore.ImportSelection, 0, len(inventory.groups[packID])),
		}
		for _, ref := range inventory.groups[packID] {
			hash, err := packstore.ParseHash(ref.Hash)
			if err != nil {
				return nil, fmt.Errorf("backup: attachment content hash %q: %w", ref.Hash, err)
			}
			id, _ := pack.ParseBlobID(ref.Hash)
			entry := s.known[id]
			candidate.Selections = append(candidate.Selections, packstore.ImportSelection{
				Hash: hash, RawLen: ref.Size, Offset: entry.Offset,
				StoredLen: entry.StoredLen, Flags: uint8(entry.Flags),
			})
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

func (s *restoreState) commitPreparedImport(
	ctx context.Context, target PackedContentTarget, prepared *packstore.PreparedImport,
) (resultErr error) {
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	dsn := sqliteURIDSN(filepath.Join(s.target, s.dbRead), "_busy_timeout=5000&mode=rw")
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return fmt.Errorf("backup: opening staged database for packed content catalog: %w", err)
	}
	db.SetMaxOpenConns(1)
	defer func() { resultErr = errors.Join(resultErr, db.Close()) }()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("backup: opening staged database for packed content catalog: %w", err)
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode=DELETE").Scan(&journalMode); err != nil {
		return fmt.Errorf("backup: selecting staged catalog journal mode: %w", err)
	}
	if journalMode != "delete" {
		return fmt.Errorf("backup: staged catalog journal mode is %q, want delete", journalMode)
	}
	catalog, err := target.OpenRestoreCatalog(ctx, db)
	if err != nil {
		return fmt.Errorf("backup: opening packed content restore catalog: %w", err)
	}
	if catalog == nil {
		return fmt.Errorf("backup: packed content target returned a nil restore catalog")
	}
	if err := prepared.Commit(ctx, catalog); err != nil {
		return err
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	if err := db.Close(); err != nil {
		return fmt.Errorf("backup: closing staged packed content catalog: %w", err)
	}
	if err := s.verifyHeldTarget(s.dbRead); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := s.root.Stat(s.dbRead + suffix); err == nil {
			return fmt.Errorf("backup: staged packed content catalog left SQLite sidecar %s", s.dbRead+suffix)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("backup: checking staged packed content catalog sidecar: %w", err)
		}
	}
	return nil
}
