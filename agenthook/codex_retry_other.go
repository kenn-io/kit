//go:build !unix && !windows

package agenthook

func transientCodexResourceError(error) bool { return false }
