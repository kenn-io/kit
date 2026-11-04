package taggedcandidate

type Drift struct{}      // want "type Drift must declare a package-level interface assertion for taggedcontracts.Optional"
func (Drift) ID() string { return "" }
func (Drift) Do()        {}
