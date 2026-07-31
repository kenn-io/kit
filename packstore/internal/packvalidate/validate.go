package packvalidate

import (
	"context"
	"errors"
	"fmt"
	"os"

	"go.kenn.io/kit/pack"
)

// BlobLimits applies exact caller ceilings after the pack footer is parsed.
// Unlike pack.ReaderLimits, zero is an exact zero-byte ceiling.
type BlobLimits struct {
	RawBytes    uint64
	StoredBytes uint64
}

// File takes ownership of file and verifies every entry in the sealed pack.
func File(
	ctx context.Context,
	file *os.File,
	packID string,
	opts pack.ReaderOptions,
	limits BlobLimits,
) (resultErr error) {
	reader, err := pack.NewReaderFromFileWithOptions(file, packID, nil, opts)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	entries := reader.Entries()
	seen := make(map[pack.BlobID]struct{}, len(entries))
	for _, entry := range entries {
		if _, ok := seen[entry.ID]; ok {
			return fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, entry.ID)
		}
		seen[entry.ID] = struct{}{}
		if entry.RawLen > limits.RawBytes {
			return &pack.StreamLimitError{
				Dimension: pack.StreamLimitRawBytes,
				Actual:    entry.RawLen,
				Limit:     limits.RawBytes,
			}
		}
		if entry.StoredLen > limits.StoredBytes {
			return &pack.StreamLimitError{
				Dimension: pack.StreamLimitStoredBytes,
				Actual:    entry.StoredLen,
				Limit:     limits.StoredBytes,
			}
		}
	}
	for _, entry := range entries {
		blob, err := reader.OpenBlob(ctx, entry)
		if err != nil {
			return err
		}
		if err := errors.Join(blob.Verify(), blob.Close()); err != nil {
			return err
		}
	}
	return nil
}
