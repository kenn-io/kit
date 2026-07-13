package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"go.kenn.io/kit/pack"
)

func createPortable(
	ctx context.Context, r *Repo, app App, opts CreateOptions,
) (*Manifest, error) {
	if err := validatePackExtension(app.PackFileExtension()); err != nil {
		return nil, err
	}
	format := opts.MetadataSource.Format()
	if err := validateMetadataFormat(format); err != nil {
		return nil, err
	}
	start := time.Now()
	pr := newProgressEmitter(opts.Progress)
	if opts.ZstdLevel == 0 {
		opts.ZstdLevel = pack.DefaultZstdLevel
	}
	if opts.Freezer == nil {
		opts.Freezer = NoopFreezeCoordinator{}
	}

	lock, err := r.AcquireExclusiveLock("create", opts.ForceUnlock)
	if err != nil {
		return nil, err
	}
	defer func() { _ = lock.Release() }()
	if err := r.CleanStaging(); err != nil {
		return nil, err
	}
	known, err := r.LoadBlobIndex()
	if err != nil {
		return nil, err
	}
	parent, err := r.LatestSnapshot()
	if err != nil {
		return nil, err
	}

	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Total: 1})
	snapshot, err := openMetadataSnapshot(ctx, opts.MetadataSource, opts.Freezer)
	if err != nil {
		return nil, err
	}
	snapshotOpen := true
	defer func() {
		if snapshotOpen {
			_ = snapshot.Close()
		}
	}()
	pr.emit(ProgressEvent{Stage: ProgressStageFreeze, Done: 1, Total: 1, Final: true})

	statsRaw, err := snapshot.Stats(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading portable metadata stats: %w", err)
	}
	if !json.Valid(statsRaw) {
		return nil, errors.New("backup: portable metadata stats are not valid JSON")
	}
	info, err := snapshot.ContentInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: reading portable content info: %w", err)
	}
	if info == nil {
		return nil, errors.New("backup: portable metadata snapshot returned nil content info")
	}

	appender := NewPackAppender(r, known, opts.ZstdLevel, nil, app.PackFileExtension())
	ok := false
	defer func() {
		if !ok {
			appender.Abort()
		}
	}()

	metadataReader, metadataBytes, err := snapshot.OpenMetadata(ctx)
	if err != nil {
		return nil, fmt.Errorf("backup: opening portable metadata: %w", err)
	}
	if metadataReader == nil || metadataBytes < 0 || uint64(metadataBytes) > pack.MaxRawLen { //nolint:gosec // negative checked first
		if metadataReader != nil {
			_ = metadataReader.Close()
		}
		return nil, fmt.Errorf("backup: invalid portable metadata size %d", metadataBytes)
	}
	pr.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Total: 1, BytesTotal: metadataBytes,
	})
	prepared, prepareErr := pack.PrepareBlob(ctx, metadataReader, uint64(metadataBytes), opts.ZstdLevel, //nolint:gosec // checked non-negative
		pack.AppendStreamOptions{ScratchDir: r.Path(stagingDirName)})
	closeErr := metadataReader.Close()
	if err := errors.Join(prepareErr, closeErr); err != nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		return nil, fmt.Errorf("backup: preparing portable metadata: %w", err)
	}
	metadataID := prepared.ID()
	if _, err := appender.AddPrepared(ctx, prepared); err != nil {
		return nil, err
	}
	pr.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Done: 1, Total: 1,
		BytesDone: metadataBytes, BytesTotal: metadataBytes, Final: true,
	})
	if err := snapshot.Close(); err != nil {
		return nil, fmt.Errorf("backup: closing portable metadata snapshot: %w", err)
	}
	snapshotOpen = false

	parentSeen := map[string]bool{}
	if parent != nil {
		_, parentSeen, err = LoadListRefs(r, known, parent.Attachments.Lists, nil, app.PackFileExtension())
		if err != nil {
			return nil, err
		}
	}
	shrunk := parentUnionShrank(parentSeen, info.Refs)
	captureSeen := parentSeen
	if shrunk {
		captureSeen = map[string]bool{}
	}
	capture, err := CaptureAttachments(ctx, opts.ContentDir, info.Refs, captureSeen, appender, CaptureOptions{
		Jobs:   opts.Jobs,
		Source: opts.ContentSource,
		Progress: func(done, total int, bytesRead int64) {
			pr.emit(ProgressEvent{
				Stage: ProgressStageAttachments, Done: int64(done), Total: int64(total), BytesDone: bytesRead,
			})
		},
	})
	if err != nil {
		return nil, err
	}
	pr.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: capture.Blobs, Total: capture.Blobs,
		BytesDone: capture.BlobBytes, BytesTotal: capture.BlobBytes, Final: true,
	})
	var lists []string
	if shrunk {
		if capture.HasNewList {
			lists = []string{capture.NewListBlob.String()}
		}
	} else {
		if parent != nil {
			lists = append(lists, parent.Attachments.Lists...)
		}
		if capture.HasNewList {
			lists = append(lists, capture.NewListBlob.String())
		}
	}

	treeBlob, hasTree, err := CaptureExtras(ctx, ExtrasOptions{
		DataDir:               opts.DataDir,
		Spec:                  opts.Extras,
		AllowPlaintextSecrets: opts.AllowPlaintextSecrets,
		Encrypted:             false,
		ContentDirName:        app.ContentDirName(),
		DBFileName:            app.DBFileName(),
	}, appender)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pr.emit(ProgressEvent{Stage: ProgressStageSeal, Total: 1})
	newPacks, newEntries, err := appender.Finish()
	if err != nil {
		return nil, err
	}
	ok = true
	pr.emit(ProgressEvent{Stage: ProgressStageSeal, Done: 1, Total: 1, Final: true})

	var bytesAdded int64
	for _, entry := range newEntries {
		bytesAdded += int64(entry.StoredLen) //nolint:gosec // stored lengths fit int64
	}
	newIndex := ""
	if len(newEntries) > 0 {
		newIndex, err = r.WriteIndex(newEntries)
		if err != nil {
			return nil, err
		}
	}
	createdAt, err := nextCreatedAt(time.Now(), parent)
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		FormatVersion:    portableMetadataManifestVersion,
		MinReaderVersion: portableMetadataManifestVersion,
		AppVersion:       app.Version(),
		CreatedAt:        createdAt.Format(time.RFC3339),
		Options: ManifestOptions{
			IncludeConfig: opts.IncludeConfig,
			IncludeTokens: opts.IncludeTokens,
			ZstdLevel:     opts.ZstdLevel,
			Tag:           opts.Tag,
		},
		Metadata: &ManifestMetadata{Format: format, Blob: metadataID.String(), Bytes: metadataBytes},
		Attachments: ManifestAttachments{
			Layout: []string{"loose"}, Rows: info.Rows, Blobs: capture.Blobs,
			BlobBytes: capture.BlobBytes, Recipes: []string{}, Lists: lists,
		},
		Excluded: app.ExcludedPaths(), Stats: statsRaw,
		NewPacks: newPacks, NewIndex: newIndex,
		DurationSeconds: time.Since(start).Seconds(), BytesAdded: bytesAdded,
	}
	if parent != nil {
		m.ParentID = parent.SnapshotID
	}
	if hasTree {
		m.Extras.Tree = treeBlob.String()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, err := r.WriteManifest(m)
	if err != nil {
		return nil, err
	}
	m.SnapshotID = id
	return m, nil
}

func openMetadataSnapshot(
	ctx context.Context, source MetadataSource, freezer FreezeCoordinator,
) (MetadataSnapshot, error) {
	if err := freezer.Begin(ctx); err != nil {
		return nil, fmt.Errorf("backup: freeze begin: %w", err)
	}
	snapshot, openErr := source.OpenSnapshot(ctx)
	endCtx, cancel := context.WithTimeout(context.Background(), freezeEndTimeout)
	endErr := freezer.End(endCtx)
	cancel()
	if openErr != nil {
		if snapshot != nil {
			_ = snapshot.Close()
		}
		return nil, errors.Join(openErr, wrapFreezeEnd(endErr))
	}
	if endErr != nil {
		_ = snapshot.Close()
		return nil, wrapFreezeEnd(endErr)
	}
	if snapshot == nil {
		return nil, errors.New("backup: metadata source returned nil snapshot")
	}
	return snapshot, nil
}

func wrapFreezeEnd(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("backup: freeze end: %w", err)
}

func validateMetadataFormat(format string) error {
	if format == "" || len(format) > 128 || strings.IndexFunc(format, unicode.IsControl) >= 0 {
		return fmt.Errorf("backup: invalid portable metadata format %q", format)
	}
	return nil
}
