package aliasassertions

import "aliascontracts"

type Correct struct{}

func (Correct) ID() string  { return "" }
func (Correct) Key() string { return "" }
func (Correct) Value() int  { return 0 }

var _ aliascontracts.Value[int] = (*Correct)(nil)

type WrongKey struct{}      // want "type WrongKey must declare a package-level interface assertion for aliascontracts.Value"
func (WrongKey) ID() string { return "" }
func (WrongKey) Key() int   { return 0 }
func (WrongKey) Value() int { return 0 }

var _ interface {
	Key() int
	Value() int
} = WrongKey{}
