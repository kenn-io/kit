package candidate

type Drift struct{}      // want "type Drift must declare a package-level interface assertion for contracts.Optional"
func (Drift) ID() string { return "" }
func (Drift) Do()        {}
