//go:build unix

package agenthook

import (
	"errors"
	"syscall"
)

func transientCodexResourceError(err error) bool {
	return errors.Is(err, syscall.EAGAIN) || errors.Is(err, syscall.ETXTBSY) ||
		errors.Is(err, syscall.EMFILE) || errors.Is(err, syscall.ENFILE) || errors.Is(err, syscall.ENOMEM)
}
