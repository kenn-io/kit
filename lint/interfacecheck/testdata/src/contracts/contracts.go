package contracts

import "time"

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
