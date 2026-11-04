package contracts

type Provider interface{ ID() string }
type Optional interface{ Do() error }
