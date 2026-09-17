// Package require is a stub of testify/require for the analyzer fixtures.
package require

import "time"

type TestingT interface{ Errorf(string, ...any) }

func Eventually(t TestingT, cond func() bool, waitFor, tick time.Duration, msgAndArgs ...any) {}

func EventuallyWithT(t TestingT, cond func(*CollectT), waitFor, tick time.Duration, msgAndArgs ...any) {
}

func Never(t TestingT, cond func() bool, waitFor, tick time.Duration, msgAndArgs ...any) {}

type CollectT struct{}

type Assertions struct{}

func New(t TestingT) *Assertions { return &Assertions{} }

func (a *Assertions) Eventually(cond func() bool, waitFor, tick time.Duration, msgAndArgs ...any) {}
