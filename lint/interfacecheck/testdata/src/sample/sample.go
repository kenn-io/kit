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
