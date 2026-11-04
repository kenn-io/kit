package interfacecheck

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAssertions(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), Analyzer, "sample", "unmarked", "transitive", "multiplebases")
}
