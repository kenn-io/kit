package interfacecheck

import (
	"testing"

	"golang.org/x/tools/go/analysis/analysistest"
)

func TestAssertions(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), New([]string{"contracts.Provider"}, []string{"contracts"}), "sample")
}

func TestWithoutContractImport(t *testing.T) {
	t.Setenv("GOPATH", analysistest.TestData())
	t.Setenv("GO111MODULE", "off")
	t.Setenv("GOWORK", "off")
	analysistest.Run(t, analysistest.TestData(), New([]string{"contracts.Provider", "contracts.TypedProvider"}, []string{"contracts"}), "noimport")
}
