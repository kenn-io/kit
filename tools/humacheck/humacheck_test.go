package humacheck

import (
	"go/ast"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/tools/go/packages"
)

// loadFixture loads GOPATH-style fixture packages from testdata/src.
func loadFixture(t *testing.T, patterns ...string) []*packages.Package {
	t.Helper()
	dir, err := filepath.Abs("testdata")
	require.NoError(t, err)
	cfg := &packages.Config{
		Mode: loadMode,
		Dir:  dir,
		Env:  append(os.Environ(), "GOPATH="+dir, "GO111MODULE=off", "GOWORK=off", "GOFLAGS=", "GOPROXY=off"),
	}
	pkgs, err := packages.Load(cfg, patterns...)
	require.NoError(t, err)
	require.NoError(t, packageErrors(pkgs))
	return pkgs
}

type expectation struct {
	file    string
	line    int
	pattern *regexp.Regexp
}

var wantPattern = regexp.MustCompile(`// want "((?:[^"\\]|\\.)*)"`)

// expectations parses `// want "regexp"` comments from fixture sources.
func expectations(t *testing.T, pkgs []*packages.Package) []expectation {
	t.Helper()
	var wants []expectation
	for _, pkg := range pkgs {
		for _, file := range pkg.Syntax {
			for _, group := range file.Comments {
				for _, comment := range group.List {
					m := wantPattern.FindStringSubmatch(comment.Text)
					if m == nil {
						continue
					}
					pos := pkg.Fset.Position(comment.Pos())
					wants = append(wants, expectation{
						file:    filepath.Base(pos.Filename),
						line:    pos.Line,
						pattern: regexp.MustCompile(strings.ReplaceAll(m[1], `\"`, `"`)),
					})
				}
			}
		}
	}
	return wants
}

// assertDiagnostics checks that every expectation is met exactly once and
// nothing else was reported.
func assertDiagnostics(t *testing.T, pkgs []*packages.Package, diags []Diagnostic) {
	t.Helper()
	wants := expectations(t, pkgs)
	matched := make([]bool, len(wants))
	for _, d := range diags {
		found := false
		for i, w := range wants {
			if matched[i] || w.file != filepath.Base(d.Path) || w.line != d.Line || !w.pattern.MatchString(d.Message) {
				continue
			}
			matched[i] = true
			found = true
			break
		}
		assert.True(t, found, "unexpected diagnostic %s:%d: %s (%s)", filepath.Base(d.Path), d.Line, d.Message, d.Rule)
	}
	for i, w := range wants {
		assert.True(t, matched[i], "missing diagnostic at %s:%d matching %q", w.file, w.line, w.pattern)
	}
}

func goOnly(pkgs []*packages.Package) []Diagnostic {
	return check(pkgs, fstest.MapFS{}, nil, "go.mod", []string{RuleSpec, RuleGenerator, RuleFrontend})
}

func TestJSONV2Rule(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{
		"v2inline", "v2helper", "v2global", "v2wrapper", "v2mixed",
		"v1default", "v1formats", "v1codec", "unknowncfg",
	} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			pkgs := loadFixture(t, fixture+"/...")
			assertDiagnostics(t, pkgs, goOnly(pkgs))
		})
	}
}

func TestClientRule(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "routes/...")
	assertDiagnostics(t, pkgs, goOnly(pkgs))
}

func TestCollectRoutes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	pkgs := loadFixture(t, "routes/server")
	routes := collectRoutes(newProgram(pkgs))
	assert.Equal([]string{"/api/v1"}, routes.adapterPrefixes)
	assert.Equal([]string{"/v2"}, routes.groupPrefixes)
	assert.ElementsMatch([]string{"/ping", "/accounts/{id}", "/jobs", "/jobs/{id}/review", "/queue", "/raw/{id}", "/grouped"}, routes.paths)
	assert.Contains(routes.concrete, "/api/v1/v2/grouped")
	assert.NotContains(routes.concrete, "/v2/api/v1/grouped")
	assert.NotContains(routes.concrete, "/grouped")
}

func TestConstructionSitesSkipTestsAndGenerated(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "routes/...")
	prog := newProgram(pkgs)
	skipped := 0
	for _, pkg := range prog.pkgs {
		for _, file := range pkg.Syntax {
			if prog.skipFile(file) {
				skipped++
			}
		}
	}
	assert.Equal(t, 1, skipped, "generated client file is skipped; the _test.go file is not loaded")
	sites := prog.constructionSites(newJSONV2Checker(prog))
	require.Len(t, sites, 1)
	assert.IsType(t, &ast.CallExpr{}, sites[0].call)
}

func TestRunRejectsUnknownRule(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), Options{Disabled: []string{"bogus"}})
	require.ErrorContains(t, err, `unknown rule "bogus"`)
}
