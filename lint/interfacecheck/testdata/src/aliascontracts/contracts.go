package aliascontracts

type Provider interface{ ID() string }
type generic[K, V any] interface {
	Key() K
	Value() V
}
type Value[T any] = generic[string, T]
