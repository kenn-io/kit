package packstore

import (
	"fmt"
)

// MaintainerOptions configures reusable physical maintenance.
type MaintainerOptions struct {
	Limits      Limits
	Coordinator *Coordinator
	Store       StoreOptions
}

// Maintainer owns physical repair, pack, unpack, and repack lifecycle work.
type Maintainer struct {
	catalog              Catalog
	layout               Layout
	limits               Limits
	coordinator          *Coordinator
	store                *Store
	packedSourcePinLimit int
	openIdentityPin      identityPinOpener
	beforeCandidatePath  func(int)
}

// NewMaintainer constructs a lifecycle engine over an application catalog.
func NewMaintainer(catalog Catalog, layout Layout, opts MaintainerOptions) (*Maintainer, error) {
	if catalog == nil {
		return nil, fmt.Errorf("packstore: catalog is nil")
	}
	if opts.Limits == (Limits{}) {
		opts.Limits = DefaultLimits()
	}
	if err := validateLimits(opts.Limits); err != nil {
		return nil, err
	}
	if opts.Coordinator == nil {
		opts.Coordinator = NewCoordinator()
	}
	opts.Store.Limits = opts.Limits
	store, err := NewStore(catalog, layout, opts.Store)
	if err != nil {
		return nil, err
	}
	return &Maintainer{
		catalog: catalog, layout: layout, limits: opts.Limits, coordinator: opts.Coordinator, store: store,
		packedSourcePinLimit: defaultPackedSourcePinLimit(opts.Limits.PackEntries),
		openIdentityPin:      openLooseIdentityPin,
	}, nil
}

// Store returns the shared mixed reader owned by this maintainer.
func (m *Maintainer) Store() *Store { return m.store }

// Close releases cached readers.
func (m *Maintainer) Close() error { return m.store.Close() }
