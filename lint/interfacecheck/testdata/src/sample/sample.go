package sample

import (
	"contracts"
	"time"
)

type Drift struct{}                   // want "type Drift must declare a package-level interface assertion for contracts.AutoMerge" "type Drift must declare a package-level interface assertion for contracts.OtherMerge"
func (*Drift) ID() string             { return "" }
func (*Drift) EnableAutoMerge() error { return nil }

type Correct struct{}

func (Correct) ID() string                          { return "" }
func (Correct) EnableAutoMerge() (time.Time, error) { return time.Time{}, nil }
func (Correct) DisableAutoMerge() error             { return nil }

var _ contracts.AutoMerge = (*Correct)(nil)
var _ contracts.OtherMerge = Correct{}

type Transport struct{}

func (*Transport) EnableAutoMerge() error { return nil }

type Multiple struct{}                               // want "type Multiple must declare a package-level interface assertion for contracts.OtherMerge"
func (Multiple) ID() string                          { return "" }
func (Multiple) EnableAutoMerge() (time.Time, error) { return time.Time{}, nil }
func (Multiple) DisableAutoMerge() error             { return nil }

var _ contracts.AutoMerge = new(Multiple)

// Inherited optional methods do not count as declared methods.
type Inherited struct{ Correct }

type Alias = Correct

type BaseOnly struct{}

func (*BaseOnly) ID() string { return "" }

type Evidence struct{} // want "type Evidence must declare a package-level interface assertion for contracts.EvidenceReader"

func (*Evidence) ID() string          { return "" }
func (*Evidence) ReadEvidence() error { return nil }

// Private methods in different packages have distinct identities.
type PrivateMethod struct{}

func (PrivateMethod) ID() string { return "" }
func (PrivateMethod) optional()  {}

type GenericCorrect struct{}

func (GenericCorrect) ID() string    { return "" }
func (GenericCorrect) Value() string { return "" }

var _ contracts.Generic[string] = GenericCorrect{}

type GenericDrift struct{}        // want "type GenericDrift must declare a package-level interface assertion for contracts.Generic"
func (GenericDrift) ID() string   { return "" }
func (GenericDrift) Value() error { return nil }

// Interface conversions are compile-time assertions too.
type Converted struct{}

func (Converted) ID() string    { return "" }
func (Converted) Value() string { return "" }

var _ = contracts.Generic[string](Converted{})

type NestedConversion struct{}

func (NestedConversion) ID() string    { return "" }
func (NestedConversion) Value() string { return "" }

var _ contracts.Generic[string] = contracts.Generic[string]((*NestedConversion)(nil))

// A multi-valued initializer asserts both concrete result types.
type TupleFirst struct{}

func (TupleFirst) ID() string    { return "" }
func (TupleFirst) Value() string { return "" }

type TupleSecond struct{}

func (TupleSecond) ID() string          { return "" }
func (TupleSecond) Value() string       { return "" }
func tuple() (TupleFirst, *TupleSecond) { return TupleFirst{}, nil }

var _, _ contracts.Generic[string] = tuple()

// Widening a checked interface to any retains the compile-time assertion.
type Widened struct{}

func (Widened) ID() string    { return "" }
func (Widened) Value() string { return "" }

var _ = any(contracts.Generic[string](Widened{}))
