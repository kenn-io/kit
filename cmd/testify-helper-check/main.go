package main

import (
	"github.com/kenn-io/kit/tools/testifyhelpercheck"
	"golang.org/x/tools/go/analysis/singlechecker"
)

func main() {
	singlechecker.Main(testifyhelpercheck.Analyzer)
}
