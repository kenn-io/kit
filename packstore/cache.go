package packstore

import (
	"errors"
	"fmt"

	"go.kenn.io/kit/pack"
)

func (s *Store) readerLocked(packID string) (*ordinaryPackReader, error) {
	if reader, ok := s.readers[packID]; ok {
		return reader, nil
	}
	if bounded, ok := s.boundedReaders[packID]; ok {
		if err := bounded.Close(); err != nil {
			return nil, fmt.Errorf("packstore: close bounded pack %s before ordinary read: %w", packID, err)
		}
		delete(s.boundedReaders, packID)
	}
	reader, err := pack.OpenReader(s.layout.PackPath(packID), nil)
	if err != nil {
		return nil, fmt.Errorf("packstore: open pack %s: %w", packID, err)
	}
	entries := reader.Entries()
	indexes := make(map[pack.BlobID]int, len(entries))
	for i, entry := range entries {
		if _, duplicate := indexes[entry.ID]; duplicate {
			_ = reader.Close()
			return nil, fmt.Errorf("%w: duplicate blob id %s", pack.ErrCorrupt, entry.ID)
		}
		indexes[entry.ID] = i
	}
	s.addPackSlotLocked(packID)
	result := &ordinaryPackReader{Reader: reader, entryIndexes: indexes}
	s.readers[packID] = result
	return result, nil
}

func (s *Store) boundedReaderLocked(packID string) (*boundedPackReader, error) {
	if reader, ok := s.boundedReaders[packID]; ok {
		return reader, nil
	}
	if ordinary, ok := s.readers[packID]; ok {
		if err := ordinary.Close(); err != nil {
			return nil, fmt.Errorf("packstore: close ordinary pack %s before bounded read: %w", packID, err)
		}
		delete(s.readers, packID)
	}
	reader, err := openBoundedPack(s.layout.PackPath(packID), s.limits)
	if err != nil {
		return nil, fmt.Errorf("packstore: open bounded pack %s: %w", packID, err)
	}
	s.addPackSlotLocked(packID)
	s.boundedReaders[packID] = reader
	return reader, nil
}

func (s *Store) addPackSlotLocked(packID string) {
	if _, ok := s.readers[packID]; ok {
		return
	}
	if _, ok := s.boundedReaders[packID]; ok {
		return
	}
	if len(s.order) >= s.slots {
		oldest := s.order[0]
		s.order = s.order[1:]
		_ = s.closePackSlotLocked(oldest)
	}
	s.order = append(s.order, packID)
}

func (s *Store) closePackSlotLocked(packID string) error {
	var closeErr error
	if reader, ok := s.readers[packID]; ok {
		closeErr = errors.Join(closeErr, reader.Close())
		delete(s.readers, packID)
	}
	if reader, ok := s.boundedReaders[packID]; ok {
		closeErr = errors.Join(closeErr, reader.Close())
		delete(s.boundedReaders, packID)
	}
	return closeErr
}
