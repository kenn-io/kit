package interfacecheck

import (
	"go/build"
	"path/filepath"
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

func TestTaggedContractLoad(t *testing.T) {
	t.Setenv("GOPATH", analysistest.TestData())
	t.Setenv("GO111MODULE", "off")
	t.Setenv("GOWORK", "off")
	saved := build.Default.BuildTags
	build.Default.BuildTags = []string{"interfacechecktest"}
	t.Cleanup(func() { build.Default.BuildTags = saved })
	analysistest.Run(t, analysistest.TestData(), New([]string{"taggedcontracts.Provider"}, []string{"taggedcontracts"}), "taggedcandidate")
}

func TestFixedGenericAliases(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), New([]string{"genericcontracts.Provider"}, []string{"genericcontracts"}), "genericassertions")
}

func TestModuleOutsideWorkingDirectory(t *testing.T) {
	analysistest.Run(t, filepath.Join(analysistest.TestData(), "modules", "outside"), New([]string{"example.org/contracts.Provider"}, []string{"example.org/contracts"}), "./candidate")
}

func TestGenericAliases(t *testing.T) {
	analysistest.Run(t, analysistest.TestData(), New([]string{"aliascontracts.Provider"}, []string{"aliascontracts"}), "aliasassertions")
}
