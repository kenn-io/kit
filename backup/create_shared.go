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
) (*AttachmentCapture, []string, ManifestExtras, error) {
	parentSeen := map[string]bool{}
	type listSegment struct {
		blob string
		refs []ContentRef
	}
	var segments []listSegment
	var segmentSizes []int
	if parent != nil {
		for _, list := range parent.Attachments.Lists {
			segment, _, err := LoadListRefs(r, known, []string{list}, nil, app.PackFileExtension())
			if err != nil {
				return nil, nil, ManifestExtras{}, err
			}
			segments = append(segments, listSegment{blob: list, refs: segment})
			segmentSizes = append(segmentSizes, len(segment))
			for _, ref := range segment {
				parentSeen[ref.Hash] = true
			}
		}
		if opts.ContentSource != nil {
			// Reuse recorded sizes to keep unchanged chunked objects on the
			// same capture path. Capture still reads and verifies their bytes.
			sizes := make(map[string]int64, len(parentSeen))
			for _, segment := range segments {
				for _, ref := range segment.refs {
					sizes[ref.Hash] = ref.Size
				}
			}
			for i := range info.Refs {
				if info.Refs[i].Size == -1 {
					if size, ok := sizes[info.Refs[i].Hash]; ok {
						info.Refs[i].Size = size
					}
				}
			}
		}
	}
	// Keep a prefix of the parent's segments and write every other current ref
	// as one new segment. The union stays exact because a kept prefix is a
	// subset of the current refs unless the parent union shrank, in which case
	// nothing is kept and the new segment is a fresh full list.
	keep := 0
	if !parentUnionShrank(parentSeen, info.Refs) {
		newRefs := 0
		for _, ref := range info.Refs {
			if !parentSeen[ref.Hash] {
				newRefs++
			}
		}
		keep = keptListSegments(segmentSizes, newRefs)
	}
	var lists []string
	captureSeen := parentSeen
	if keep < len(segments) {
		captureSeen = map[string]bool{}
	}
	for i, segment := range segments {
		if i == keep {
			break
		}
		lists = append(lists, segment.blob)
		if keep < len(segments) {
			for _, ref := range segment.refs {
				captureSeen[ref.Hash] = true
			}
		}
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
		return nil, nil, ManifestExtras{}, err
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageAttachments, Done: capture.Blobs, Total: capture.Blobs,
		BytesDone: capture.BlobBytes, BytesTotal: capture.BlobBytes, Final: true,
	})
	if capture.HasNewList {
		lists = append(lists, capture.NewListBlob.String())
	}

	extras, err := CaptureExtras(ctx, ExtrasOptions{
		DataDir:               opts.DataDir,
		Spec:                  opts.Extras,
		AllowPlaintextSecrets: opts.AllowPlaintextSecrets,
		Encrypted:             false,
		ContentDirName:        app.ContentDirName(),
		DBFileName:            app.DBFileName(),
	}, appender)
	if err != nil {
		return nil, nil, ManifestExtras{}, err
	}
	return capture, lists, extras, nil
}

func preparePortableMetadata(
	ctx context.Context,
	snapshot MetadataSnapshot,
	appender *PackAppender,
	progress *progressEmitter,
) (pack.BlobID, int64, string, error) {
	metadataReader, metadataBytes, err := snapshot.OpenMetadata(ctx)
	if err != nil {
		if metadataReader != nil {
			err = errors.Join(err, metadataReader.Close())
		}
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: opening portable metadata: %w", err)
	}
	if metadataReader == nil || metadataBytes < 0 || metadataBytes > MaxObjectBytes {
		if metadataReader != nil {
			_ = metadataReader.Close()
		}
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: invalid portable metadata size %d", metadataBytes)
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Total: 1, BytesTotal: metadataBytes,
	})
	metadataID, size, recipe, captureErr := captureObject(ctx, metadataReader, metadataBytes, nil, appender)
	if err := errors.Join(captureErr, metadataReader.Close()); err != nil {
		return pack.BlobID{}, 0, "", fmt.Errorf("backup: preparing portable metadata: %w", err)
	}
	progress.emit(ProgressEvent{
		Stage: ProgressStageMetadata, Done: 1, Total: 1,
		BytesDone: metadataBytes, BytesTotal: metadataBytes, Final: true,
	})
	return metadataID, size, recipe, nil
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
		bytesAdded += int64(entry.StoredLen)
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
