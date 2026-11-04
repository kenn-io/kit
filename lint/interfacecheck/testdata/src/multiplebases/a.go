package multiplebases

//kit:interfacecheck
type First interface{ ID() string } // want First:".*"
//kit:interfacecheck
type Second interface{ ID() int } // want Second:".*"
//kit:interfacecheck
type Third interface{ Key() string } // want Third:".*"

type Optional interface{ Read() }

type Candidate struct{}       // want "type Candidate must declare a package-level interface assertion for multiplebases.Optional"
func (Candidate) ID() string  { return "" }
func (Candidate) Key() string { return "" }
func (Candidate) Read()       {}
