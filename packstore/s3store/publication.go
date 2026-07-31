package s3store

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"go.kenn.io/kit/pack"
	"go.kenn.io/kit/packstore"
)

// PublishLoose conditionally publishes immutable logical bytes at their
// canonical raw key, then performs an independent full GET and SHA-256
// verification. ETags never grant content authority.
func (b *Backend) PublishLoose(
	ctx context.Context,
	hash packstore.Hash,
	src io.Reader,
	opts packstore.PublishOptions,
) (packstore.LooseReceipt, error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err := hash.Validate(); err != nil {
		return packstore.LooseReceipt{}, err
	}
	if src == nil {
		return packstore.LooseReceipt{}, fmt.Errorf("s3store: nil loose publication source")
	}
	if opts.Compression.Enabled {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: compressed loose publication is not supported",
		)
	}
	key := b.keys.loose(hash, packstore.LooseEncodingRaw)
	result, err := b.multipartPublish(ctx, key, src, opts.MaxBytes, hash.String())
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if opts.SizeKnown && result.size != opts.ExpectedSize {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"%w: published size %d does not match expected %d",
			packstore.ErrContentMismatch, result.size, opts.ExpectedSize,
		)
	}
	if hex.EncodeToString(result.digest[:]) != hash.String() {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"%w: published bytes differ from %s", packstore.ErrContentMismatch, hash,
		)
	}
	if err := b.verifyRawObject(ctx, key, hash, result.size); err != nil {
		return packstore.LooseReceipt{}, err
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	return packstore.LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: hash,
		Location: packstore.LooseLocation{
			Encoding: packstore.LooseEncodingRaw, LogicalSize: result.size,
			StoredSize: result.size,
		},
		Created: result.created,
	}, nil
}

// RepairLoose deliberately replaces one damaged canonical loose object after
// validating the complete source locally, then independently reads it back.
func (b *Backend) RepairLoose(
	ctx context.Context,
	hash packstore.Hash,
	src io.Reader,
	opts packstore.PublishOptions,
) (receipt packstore.LooseReceipt, resultErr error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	if err := hash.Validate(); err != nil {
		return packstore.LooseReceipt{}, err
	}
	if src == nil || !opts.SizeKnown || opts.ExpectedSize < 0 ||
		opts.Compression.Enabled {
		return packstore.LooseReceipt{}, packstore.ErrInvalidPolicy
	}
	staged, err := os.CreateTemp("", "kit-s3-repair-*")
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: create repair staging: %w", err,
		)
	}
	path := staged.Name()
	defer func() {
		resultErr = errors.Join(resultErr, staged.Close())
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if err := staged.Chmod(0o600); err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: protect repair staging: %w", err,
		)
	}
	hasher := sha256.New()
	reader := io.Reader(src)
	if opts.MaxBytes > 0 {
		reader = io.LimitReader(src, opts.MaxBytes+1)
	}
	size, err := io.CopyBuffer(
		io.MultiWriter(staged, hasher), reader, make([]byte, 64<<10),
	)
	if err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: stage repair source: %w", err,
		)
	}
	if (opts.MaxBytes > 0 && size > opts.MaxBytes) ||
		size != opts.ExpectedSize ||
		hex.EncodeToString(hasher.Sum(nil)) != hash.String() {
		return packstore.LooseReceipt{}, packstore.ErrContentMismatch
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return packstore.LooseReceipt{}, fmt.Errorf(
			"s3store: rewind repair source: %w", err,
		)
	}
	key := b.keys.loose(hash, packstore.LooseEncodingRaw)
	if _, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body: staged, ContentLength: aws.Int64(size),
	}); err != nil {
		return packstore.LooseReceipt{}, classifyError("replace loose object", err)
	}
	if err := b.verifyRawObject(ctx, key, hash, size); err != nil {
		return packstore.LooseReceipt{}, err
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.LooseReceipt{}, err
	}
	return packstore.LooseReceipt{
		StoreID: owner.Store, Generation: generation, Hash: hash,
		Location: packstore.LooseLocation{
			Encoding: packstore.LooseEncodingRaw, LogicalSize: size,
			StoredSize: size,
		},
		Created: false,
	}, nil
}

// PublishPack conditionally publishes a sealed pack, then independently reads
// it back and verifies every entry before returning physical evidence.
func (b *Backend) PublishPack(
	ctx context.Context,
	packID string,
	src io.Reader,
	opts packstore.PublishOptions,
) (packstore.PackReceipt, error) {
	owner, err := b.requireOwnership(ctx)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	if !pack.IsValidPackID(packID) {
		return packstore.PackReceipt{}, fmt.Errorf("s3store: invalid pack id %q", packID)
	}
	if src == nil {
		return packstore.PackReceipt{}, fmt.Errorf("s3store: nil pack publication source")
	}
	result, err := b.multipartPublish(ctx, b.keys.pack(packID), src, opts.MaxBytes, "")
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	if opts.SizeKnown && result.size != opts.ExpectedSize {
		return packstore.PackReceipt{}, fmt.Errorf(
			"%w: published pack size %d does not match expected %d",
			packstore.ErrContentMismatch, result.size, opts.ExpectedSize,
		)
	}
	size, digest, err := b.verifyPackObject(ctx, packID, result.size)
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	if digest != result.digest {
		return packstore.PackReceipt{}, errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: canonical pack differs from published bytes"),
		)
	}
	generation, err := newGeneration()
	if err != nil {
		return packstore.PackReceipt{}, err
	}
	return packstore.PackReceipt{
		StoreID: owner.Store, Generation: generation, PackID: packID,
		Size: size, Created: result.created,
	}, nil
}

type publicationResult struct {
	size    int64
	digest  [sha256.Size]byte
	created bool
}

func (b *Backend) multipartPublish(
	ctx context.Context,
	key string,
	src io.Reader,
	maxBytes int64,
	expectedDigest string,
) (result publicationResult, resultErr error) {
	created, err := b.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return publicationResult{}, classifyError("begin multipart publication", err)
	}
	uploadID := aws.ToString(created.UploadId)
	completed := false
	defer func() {
		if completed {
			return
		}
		_, abortErr := b.client.AbortMultipartUpload(context.WithoutCancel(ctx),
			&s3.AbortMultipartUploadInput{
				Bucket: aws.String(b.bucket), Key: aws.String(key),
				UploadId: aws.String(uploadID),
			})
		if abortErr != nil {
			resultErr = errors.Join(resultErr, classifyError("abort multipart publication", abortErr))
		}
	}()

	hasher := sha256.New()
	buffer := make([]byte, b.part)
	var parts []types.CompletedPart
	for partNumber := int32(1); ; partNumber++ {
		n, readErr := io.ReadFull(src, buffer)
		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			readErr = io.EOF
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return publicationResult{}, fmt.Errorf("s3store: read publication source: %w", readErr)
		}
		if n == 0 {
			if len(parts) == 0 {
				return b.publishEmpty(ctx, key, expectedDigest)
			}
			break
		}
		result.size += int64(n)
		if maxBytes > 0 && result.size > maxBytes {
			return publicationResult{}, fmt.Errorf(
				"%w: publication exceeds %d bytes",
				packstore.ErrContentMismatch, maxBytes,
			)
		}
		_, _ = hasher.Write(buffer[:n])
		uploaded, err := b.client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(b.bucket), Key: aws.String(key),
			UploadId: aws.String(uploadID), PartNumber: aws.Int32(partNumber),
			Body: bytes.NewReader(buffer[:n]), ContentLength: aws.Int64(int64(n)),
		})
		if err != nil {
			return publicationResult{}, classifyError("upload multipart part", err)
		}
		parts = append(parts, types.CompletedPart{
			ETag: uploaded.ETag, PartNumber: aws.Int32(partNumber),
		})
		if errors.Is(readErr, io.EOF) {
			break
		}
	}
	copy(result.digest[:], hasher.Sum(nil))
	if expectedDigest != "" && hex.EncodeToString(result.digest[:]) != expectedDigest {
		return publicationResult{}, fmt.Errorf(
			"%w: publication bytes differ from %s",
			packstore.ErrContentMismatch, expectedDigest,
		)
	}
	_, err = b.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		UploadId: aws.String(uploadID), IfNoneMatch: aws.String("*"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	switch statusCode(err) {
	case 0:
		if err != nil {
			return publicationResult{}, classifyError("complete multipart publication", err)
		}
		result.created = true
		completed = true
	case http.StatusPreconditionFailed:
		result.created = false
		return result, nil
	default:
		return publicationResult{}, classifyError("complete multipart publication", err)
	}
	return result, nil
}

func (b *Backend) publishEmpty(
	ctx context.Context,
	key string,
	expectedDigest string,
) (publicationResult, error) {
	digest := sha256.Sum256(nil)
	if expectedDigest != "" && hex.EncodeToString(digest[:]) != expectedDigest {
		return publicationResult{}, fmt.Errorf(
			"%w: publication bytes differ from %s",
			packstore.ErrContentMismatch, expectedDigest,
		)
	}
	_, err := b.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
		Body: bytes.NewReader(nil), IfNoneMatch: aws.String("*"),
	})
	if err != nil && statusCode(err) != http.StatusPreconditionFailed {
		return publicationResult{}, classifyError("publish empty object", err)
	}
	return publicationResult{
		digest: digest, created: err == nil,
	}, nil
}

func (b *Backend) verifyRawObject(
	ctx context.Context,
	key string,
	hash packstore.Hash,
	expectedSize int64,
) error {
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(key),
	})
	if err != nil {
		return classifyError("read back loose object", err)
	}
	defer func() { _ = output.Body.Close() }()
	if err := validateReadbackSize("loose", output.ContentLength, expectedSize); err != nil {
		return err
	}
	hasher := sha256.New()
	size, err := io.CopyBuffer(
		hasher,
		boundedReadback(output.Body, expectedSize),
		make([]byte, 64<<10),
	)
	if err != nil {
		return classifyError("read back loose object", err)
	}
	if size != expectedSize || hex.EncodeToString(hasher.Sum(nil)) != hash.String() {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: loose read-back does not match canonical identity"),
		)
	}
	return nil
}

func (b *Backend) verifyPackObject(
	ctx context.Context,
	packID string,
	expectedSize int64,
) (resultSize int64, digest [sha256.Size]byte, resultErr error) {
	if expectedSize > b.limits.PackBytes {
		return 0, digest, &packstore.LimitError{
			Dimension: packstore.LimitPackContainerBytes,
			Actual:    uint64(expectedSize),       //nolint:gosec // checked non-negative below
			Limit:     uint64(b.limits.PackBytes), //nolint:gosec // validated positive
		}
	}
	output, err := b.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(b.bucket), Key: aws.String(b.keys.pack(packID)),
	})
	if err != nil {
		return 0, digest, classifyError("read back pack", err)
	}
	defer func() { resultErr = errors.Join(resultErr, output.Body.Close()) }()
	if err := validateReadbackSize("pack", output.ContentLength, expectedSize); err != nil {
		return 0, digest, err
	}
	staged, err := os.CreateTemp("", "kit-s3-pack-*")
	if err != nil {
		return 0, digest, fmt.Errorf("s3store: create pack verification staging: %w", err)
	}
	stagedPath := staged.Name()
	defer func() {
		if staged != nil {
			resultErr = errors.Join(resultErr, staged.Close())
		}
		if err := os.Remove(stagedPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	hasher := sha256.New()
	resultSize, err = io.CopyBuffer(
		io.MultiWriter(staged, hasher),
		boundedReadback(output.Body, expectedSize),
		make([]byte, 64<<10),
	)
	if err != nil {
		return 0, digest, classifyError("read back pack", err)
	}
	copy(digest[:], hasher.Sum(nil))
	if resultSize != expectedSize {
		return 0, digest, errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: pack read-back size %d differs from %d", resultSize, expectedSize),
		)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return 0, digest, err
	}
	reader, err := pack.NewReaderFromFileWithOptions(
		staged,
		packID,
		nil,
		b.packReaderOptions(),
	)
	if err != nil {
		if mapped, ok := mapPackLimit(err); ok {
			return 0, digest, mapped
		}
		return 0, digest, errors.Join(packstore.ErrPhysicalCorrupt, err)
	}
	staged = nil
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	for _, entry := range reader.Entries() {
		blob, err := reader.OpenBlob(ctx, entry)
		if err != nil {
			if mapped, ok := mapPackLimit(err); ok {
				return 0, digest, mapped
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return 0, digest, err
			}
			return 0, digest, errors.Join(packstore.ErrPhysicalCorrupt, err)
		}
		if err := errors.Join(blob.Verify(), blob.Close()); err != nil {
			if mapped, ok := mapPackLimit(err); ok {
				return 0, digest, mapped
			}
			if errors.Is(err, context.Canceled) ||
				errors.Is(err, context.DeadlineExceeded) {
				return 0, digest, err
			}
			return 0, digest, errors.Join(packstore.ErrPhysicalCorrupt, err)
		}
	}
	return resultSize, digest, nil
}

func validateReadbackSize(kind string, actual *int64, expected int64) error {
	if expected < 0 {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf("s3store: invalid negative %s read-back size %d", kind, expected),
		)
	}
	if actual != nil && *actual != expected {
		return errors.Join(
			packstore.ErrPhysicalCorrupt,
			fmt.Errorf(
				"s3store: %s read-back content length %d differs from %d",
				kind,
				*actual,
				expected,
			),
		)
	}
	return nil
}

func boundedReadback(src io.Reader, expected int64) io.Reader {
	return io.MultiReader(
		io.LimitReader(src, expected),
		io.LimitReader(src, 1),
	)
}

func newGeneration() (packstore.LocationGeneration, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("s3store: generate location identity: %w", err)
	}
	return packstore.LocationGeneration(hex.EncodeToString(value[:])), nil
}
