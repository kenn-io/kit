package backup

import (
	"context"
	"errors"
	"fmt"

	"go.kenn.io/kit/pack"
)

type sealedCapture struct {
	newPacks   []string
	newIndex   string
	bytesAdded int64
}

func captureSnapshotFiles(
	ctx context.Context,
	r *Repo,
	app App,
	opts CreateOptions,
	parent *Manifest,
	known map[pack.BlobID]IndexEntry,
	info *ContentInfo,
	appender *PackAppender,
	progress *progressEmitter,
) (*AttachmentCapture, []string, pack.BlobID, bool, error) {
	parentSeen := map[string]bool{}
	if parent != nil {
		var err error
		_, parentSeen, err = LoadListRefs(
			r, known, parent.Attachments.Lists, nil, app.PackFileExtension())
		if err != nil {
			return nil, nil, pack.BlobID{}, false, err
		}
	}
	// Inherit lists only while the parent union remains a subset of the
	// current references. After shrinkage, one fresh full list must replace
	// the inherited union or Verify's population invariant would fail.
	shrunk := parentUnionShrank(parentSeen, info.Refs)
	captureSeen := parentSeen
	if shrunk {
		captureSeen = map[string]bool{}
	}
	capture, err := CaptureAttachments(
		ctx, opts.ContentDir, info.Refs, captureSeen, appender, CaptureOptions{
			Jobs:   opts.Jobs,
			Source: opts.ContentSource,
			Progress: func(done, total int, bytesRead int64) {
				progress.emit(ProgressEvent{
					Stage: ProgressStageAttachments, Done: int64(done),
					Total: int64(total), BytesDone: bytesRead,
				})
			},
		})
	if err != nil {
		return nil, nil, pack.BlobID{}, false, err
	}
	progress.emit(ProgressEvent{
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
		return nil, nil, pack.BlobID{}, false, err
	}
	return capture, lists, treeBlob, hasTree, nil
}

func preparePortableMetadata(
	ctx context.Context,
	r *Repo,
	snapshot MetadataSnapshot,
	opts CreateOptions,
	appender *PackAppender,
	progress *progressEmitter,
) (pack.BlobID, int64, error) {
	metadataReader, metadataBytes, err := snapshot.OpenMetadata(ctx)
	if err != nil {
		if metadataReader != nil {
			err = errors.Join(err, metadataReader.Close())
		}
		return pack.BlobID{}, 0, fmt.Errorf("backup: opening portable metadata: %w", err)
	}
	if metadataReader == nil || metadataBytes < 0 || uint64(metadataBytes) > pack.MaxRawLen { //nolint:gosec // negative checked first
		if metadataReader != nil {
			_ = metadataReader.Close()
		}
		return pack.BlobID{}, 0, fmt.Errorf("backup: invalid portable metadata size %d", metadataBytes)
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Total: 1, BytesTotal: metadataBytes,
	})
	prepared, prepareErr := pack.PrepareBlob(
		ctx, metadataReader, uint64(metadataBytes), opts.ZstdLevel, //nolint:gosec // checked non-negative
		pack.AppendStreamOptions{ScratchDir: r.Path(stagingDirName)})
	closeErr := metadataReader.Close()
	if err := errors.Join(prepareErr, closeErr); err != nil {
		if prepared != nil {
			_ = prepared.Close()
		}
		return pack.BlobID{}, 0, fmt.Errorf("backup: preparing portable metadata: %w", err)
	}
	metadataID := prepared.ID()
	if _, err := appender.AddPrepared(ctx, prepared); err != nil {
		return pack.BlobID{}, 0, err
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Done: 1, Total: 1,
		BytesDone: metadataBytes, BytesTotal: metadataBytes, Final: true,
	})
	return metadataID, metadataBytes, nil
}

func sealSnapshotCapture(
	ctx context.Context, r *Repo, appender *PackAppender, progress *progressEmitter,
) (sealedCapture, error) {
	if err := ctx.Err(); err != nil {
		return sealedCapture{}, err
	}
	progress.emit(ProgressEvent{Stage: ProgressStageSeal, Total: 1})
	newPacks, newEntries, err := appender.Finish()
	if err != nil {
		return sealedCapture{}, err
	}
	progress.emit(ProgressEvent{Stage: ProgressStageSeal, Done: 1, Total: 1, Final: true})

	var bytesAdded int64
	for _, entry := range newEntries {
		bytesAdded += int64(entry.StoredLen) //nolint:gosec // stored lengths fit int64
	}
	newIndex := ""
	if len(newEntries) > 0 {
		newIndex, err = r.WriteIndex(newEntries)
		if err != nil {
			return sealedCapture{}, err
		}
	}
	return sealedCapture{newPacks: newPacks, newIndex: newIndex, bytesAdded: bytesAdded}, nil
}
