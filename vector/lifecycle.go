package vector

import (
	"errors"
	"fmt"
)

// State is a generation's role in the active/building lifecycle. Only
// building and active generations are searched; building sorts ahead of
// active so Merge keeps the newer generation's hit on overlap.
type State string

const (
	StatePending  State = "pending"
	StateBuilding State = "building"
	StateActive   State = "active"
	StateRetired  State = "retired"
)

// GenerationInfo describes one registered generation.
type GenerationInfo[G comparable] struct {
	Key         G
	Fingerprint string
	Dimension   int
	State       State
}

// ErrUncovered reports that Activate refused to publish a generation
// because at least one document is not covered at its current revision.
// The state change is not committed. Callers retry after a later fill.
var ErrUncovered = errors.New("generation has uncovered documents")

// ErrGenerationNotFound reports a generation key that EnsureGeneration never
// registered.
var ErrGenerationNotFound = errors.New("generation not ensured")

// ErrRetired reports a retired generation used where only a building or
// active one is allowed. A retired generation never becomes live again.
var ErrRetired = errors.New("generation is retired")

// UncoveredError is the Activate failure for an incomplete generation.
// Backlog is the number of documents that still need a current stamp.
type UncoveredError struct {
	Backlog int64
}

func (e *UncoveredError) Error() string {
	return fmt.Sprintf("generation has %d uncovered documents", e.Backlog)
}

func (e *UncoveredError) Unwrap() error { return ErrUncovered }
