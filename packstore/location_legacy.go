package packstore

import "context"

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
	backend ReadBackend
}

func (r legacyBackendRegistry) Backend(id StoreID) (ReadBackend, bool) {
	if id != legacyStoreID {
		return nil, false
	}
	return r.backend, true
}
