package transitive

import "sample"

type Candidate struct{ sample.Correct }  // want "type Candidate must declare a package-level interface assertion for contracts.AutoMerge" "type Candidate must declare a package-level interface assertion for contracts.OtherMerge"
func (Candidate) EnableAutoMerge() error { return nil }
