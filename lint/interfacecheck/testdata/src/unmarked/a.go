package unmarked

type Base interface{ ID() string }
type Optional interface{ Read() }
type Candidate struct{}

func (Candidate) ID() string { return "" }
func (Candidate) Read()      {}
