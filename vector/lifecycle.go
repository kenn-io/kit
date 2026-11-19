package vector

import (
	"errors"
	"fmt"
)

// State is a generation's role in the active/building lifecycle.
type State string

const (
	StatePending  State = "pending"
	StateBuilding State = "building"
	StateActive   State = "active"
	StateRetired  State = "retired"
)

// ErrUncovered reports that activation was refused because a document is not
// covered at its current revision.
var ErrUncovered = errors.New("generation has uncovered documents")

// ErrGenerationNotFound reports a generation key that has not been registered.
var ErrGenerationNotFound = errors.New("generation not ensured")

// ErrRetired reports an operation that requires a live generation.
var ErrRetired = errors.New("generation is retired")

// UncoveredError reports how many documents still need a current stamp.
type UncoveredError struct {
	Backlog int64
}

func (e *UncoveredError) Error() string {
	return fmt.Sprintf("generation has %d uncovered documents", e.Backlog)
}

func (e *UncoveredError) Unwrap() error { return ErrUncovered }

// GenerationInfo describes one registered generation.
type GenerationInfo[G comparable] struct {
	Key         G
	Fingerprint string
	Dimension   int
	State       State
}
