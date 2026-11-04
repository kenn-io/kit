package contracts

import (
	"shared"
	"time"
)

type Provider interface{ ID() string }
type AutoMerge interface{ EnableAutoMerge() (time.Time, error) }
type OtherMerge interface {
	EnableAutoMerge() (time.Time, error)
	DisableAutoMerge() error
}
type hidden interface{ ID() string }

// Embedded base methods must not trigger optional interface assertions.
type EvidenceReader interface {
	Provider
	ReadEvidence() error
}

// Constraints cannot be used in variable assertions.
type NumericProvider interface {
	~int
	ID() string
}

type PrivateOptional interface{ optional() }
type Generic[T any] interface{ Value() T }

type TypedProvider interface{ Kind() shared.Kind }
type TypedOptional interface{ UseKind(shared.Kind) }
