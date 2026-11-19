package sample

// A base can be satisfied through embedding, including in test files.
type TestDrift struct{ Correct }          // want "type TestDrift must declare a package-level interface assertion for contracts.AutoMerge" "type TestDrift must declare a package-level interface assertion for contracts.OtherMerge"
func (*TestDrift) EnableAutoMerge() error { return nil }
