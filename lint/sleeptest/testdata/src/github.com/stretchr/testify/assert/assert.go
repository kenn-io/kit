// Package assert is a stub of testify/assert for the analyzer fixtures.
package assert

import "time"

type TestingT interface{ Errorf(string, ...any) }

func Eventually(t TestingT, cond func() bool, waitFor, tick time.Duration, msgAndArgs ...any) bool {
	return true
}

func Equal(t TestingT, expected, actual any, msgAndArgs ...any) bool { return true }
