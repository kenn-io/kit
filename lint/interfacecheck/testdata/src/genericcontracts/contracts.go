package genericcontracts

type Provider interface{ ID() string }
type generic[T any] interface{ Value() T }
type StringValue = generic[string]
type IntValue = generic[int]
