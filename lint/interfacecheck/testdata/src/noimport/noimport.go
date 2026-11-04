package noimport

import "shared"

type Drift struct{}                   // want "type Drift must declare a package-level interface assertion for contracts.AutoMerge" "type Drift must declare a package-level interface assertion for contracts.OtherMerge"
func (*Drift) ID() string             { return "" }
func (*Drift) EnableAutoMerge() error { return nil }

type TypedDrift struct{}             // want "type TypedDrift must declare a package-level interface assertion for contracts.TypedOptional"
func (TypedDrift) Kind() shared.Kind { return "" }
func (TypedDrift) UseKind(string)    {}
