//go:build interfacechecktest

package taggedcontracts

type Provider interface{ ID() string }
type Optional interface{ Do() error }
