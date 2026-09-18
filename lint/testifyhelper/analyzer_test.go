package testifyhelper

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAnalyzer(t *testing.T) {
	testdata := analysistest.TestData()
	analysistest.Run(t, testdata, Analyzer, "a")
}

func TestSuggestedFixes(t *testing.T) {
	t.Parallel()
	analysistest.RunWithSuggestedFixes(t, analysistest.TestData(), Analyzer, "fixes", "aliases")
}
