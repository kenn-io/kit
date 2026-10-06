package backup

import (
	"context"
	"errors"
	"io"
	"path/filepath"

	"go.kenn.io/kit/packstore"
)

func (s *restoreState) restoreCompressedContent(ctx context.Context, directory string, ref ContentRef, src io.Reader) (resultErr error) {
	root, _, err := s.openLeafDir(filepath.Join(directory, ".restore-content"))
	if err != nil {
		return err
	}
	if root != s.root {
		defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	}
	hash, err := packstore.ParseHash(ref.Hash)
	if err != nil {
		return err
	}
	receipt, err := packstore.RestoreLoose(ctx, root, hash, src, packstore.WriteOptions{
		Durability: packstore.DurablePublication, Dedup: packstore.VerifyFullHash,
		ExpectedHash: hash, ExpectedSize: ref.Size, SizeKnown: true,
		MaxBytes: MaxObjectBytes, Compression: s.compression,
	})
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.looseContent = append(s.looseContent, receipt)
	s.mu.Unlock()
	return nil
}
