package packvalidate

import (
	"context"
	"errors"
	"os"

	"go.kenn.io/kit/pack"
)

// File takes ownership of file and verifies every entry in the sealed pack.
func File(
	ctx context.Context,
	file *os.File,
	packID string,
	opts pack.ReaderOptions,
) (resultErr error) {
	reader, err := pack.NewReaderFromFileWithOptions(file, packID, nil, opts)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, reader.Close()) }()
	for _, entry := range reader.Entries() {
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
