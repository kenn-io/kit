package packstore

import (
	"context"
	"io"
)

const legacyStoreID StoreID = "local"
const legacyLocationGenerationValue LocationGeneration = "legacy"

type legacyLocationResolver struct {
	resolver Resolver
}

func (r legacyLocationResolver) ResolveLocations(
	ctx context.Context,
	hash Hash,
) (Resolution, error) {
	location, err := r.resolver.Resolve(ctx, hash)
	if err != nil {
		return Resolution{}, err
	}
	if !location.Member {
		return Resolution{}, nil
	}
	candidate := ReadLocation{
		StoreID:    legacyStoreID,
		Generation: legacyLocationGenerationValue,
	}
	if location.Pack == nil {
		candidate.Loose = &LooseLocation{}
	} else {
		candidate.Pack = location.Pack
	}
	return Resolution{Member: true, Candidates: []ReadLocation{candidate}}, nil
}

type legacyBackendRegistry struct {
	backend *legacyReadBackend
}

func (r legacyBackendRegistry) Backend(id StoreID) (ReadBackend, bool) {
	if id != legacyStoreID {
		return nil, false
	}
	return r.backend, true
}

type legacyReadBackend struct {
	store *Store
}

func (b *legacyReadBackend) OpenLoose(
	ctx context.Context,
	hash Hash,
	_ LooseLocation,
) (VerifiedReadCloser, int64, error) {
	stream, size, err := b.store.openLooseStream(ctx, hash)
	if err != nil {
		return nil, 0, classifyPhysicalError(err)
	}
	return stream, size, nil
}

func (b *legacyReadBackend) OpenPack(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
) (VerifiedReadCloser, int64, error) {
	stream, size, err := b.store.openPackedStream(ctx, hash, &entry)
	if err != nil {
		return nil, 0, classifyPhysicalError(err)
	}
	return stream, size, nil
}

func (b *legacyReadBackend) OpenSeekableLoose(
	ctx context.Context,
	hash Hash,
	_ LooseLocation,
) (io.ReadSeekCloser, int64, error) {
	reader, size, err := b.store.openSeekableLoose(ctx, hash)
	return reader, size, classifyPhysicalError(err)
}

func (b *legacyReadBackend) OpenSeekablePack(
	_ context.Context,
	hash Hash,
	entry IndexEntry,
) (io.ReadSeekCloser, int64, error) {
	reader, size, err := b.store.openPacked(hash, &entry)
	return reader, size, classifyPhysicalError(err)
}

func (b *legacyReadBackend) ReadLooseBounded(
	ctx context.Context,
	hash Hash,
	_ LooseLocation,
	maxBytes int64,
) ([]byte, int64, error) {
	data, size, err := b.store.readLooseBounded(ctx, hash, maxBytes)
	return data, size, classifyPhysicalError(err)
}

func (b *legacyReadBackend) ReadPackBounded(
	ctx context.Context,
	hash Hash,
	entry IndexEntry,
	maxBytes int64,
) ([]byte, int64, error) {
	data, size, err := b.store.readPackedBounded(ctx, hash, &entry, maxBytes)
	return data, size, classifyPhysicalError(err)
}
