package genericassertions

import "genericcontracts"

type Fixed struct{}         // want "type Fixed must declare a package-level interface assertion for genericcontracts.IntValue"
func (Fixed) ID() string    { return "" }
func (Fixed) Value() string { return "" }

var _ genericcontracts.StringValue = Fixed{}
