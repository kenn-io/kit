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

	gittest "go.kenn.io/kit/git/test"
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

func goOnly(t *testing.T, pkgs []*packages.Package) []Diagnostic {
	t.Helper()
	diags, err := check(pkgs, fstest.MapFS{}, nil, "go.mod", []string{RuleSpec, RuleGenerator, RuleFrontend})
	require.NoError(t, err)
	return diags
}

func TestJSONV2Rule(t *testing.T) {
	t.Parallel()
	for _, fixture := range []string{"v2inline", "v2global", "v1import"} {
		t.Run(fixture, func(t *testing.T) {
			t.Parallel()
			pkgs := loadFixture(t, fixture+"/...")
			assertDiagnostics(t, pkgs, goOnly(t, pkgs))
		})
	}
}

func TestJSONV2MissingInstall(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "nov2/...")
	diags := goOnly(t, pkgs)
	require.Len(t, diags, 1)
	assert.Equal(t, Diagnostic{Path: "go.mod", Line: 1, Column: 1, Rule: RuleJSONV2, Message: jsonV2MissingMessage}, diags[0])
}

// TestRunUsesStagedGoSources stages a file with a v1 import, then removes the
// import in the working tree only: the diagnostic must still be reported.
func TestRunUsesStagedGoSources(t *testing.T) {
	t.Parallel()
	repo := gittest.NewRepo(t, gittest.Options{ResolvePath: true})
	repo.WriteFile("go.mod", "module m\n\ngo "+goVersion(t)+"\n")
	repo.WriteFile("m.go", "package m\n\nimport \"encoding/json\"\n\nvar _ = json.Marshal\n")
	repo.Run("add", "-A")
	repo.WriteFile("m.go", "package m\n")

	diags, err := Run(t.Context(), Options{Dir: repo.Root})
	require.NoError(t, err)
	require.Len(t, diags, 1)
	assert.Equal(t, Diagnostic{Path: "m.go", Line: 3, Column: 8, Rule: RuleJSONV2, Message: jsonV1ImportMessage}, diags[0])
}

// goVersion returns the go directive of this module so fixture modules load
// with the same toolchain.
func goVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile("../../go.mod")
	require.NoError(t, err)
	m := regexp.MustCompile(`(?m)^go (\S+)`).FindStringSubmatch(string(data))
	require.NotNil(t, m)
	return m[1]
}

func TestClientRule(t *testing.T) {
	t.Parallel()
	pkgs := loadFixture(t, "routes/...")
	assertDiagnostics(t, pkgs, goOnly(t, pkgs))
}

func TestCollectRoutes(t *testing.T) {
	t.Parallel()
	assert := assert.New(t)
	pkgs := loadFixture(t, "routes/server")
	routes := collectRoutes(newProgram(pkgs))
	assert.Equal([]string{"/api/v1"}, routes.adapterPrefixes)
	assert.Equal([]string{"/v2"}, routes.groupPrefixes)
	assert.ElementsMatch([]string{"/ping", "/accounts/{id}", "/jobs", "/jobs/{id}/review", "/queue", "/raw/{id}", "/grouped"}, routes.paths)
	assert.Equal(receiverGroup, routes.kinds["/grouped"])
	assert.Equal(receiverAPI, routes.kinds["/ping"])
	assert.Equal(receiverAPI, routes.kinds["/raw/{id}"], "the helper is called with a plain API")
	assert.Contains(routes.concrete, "/api/v1/v2/grouped")
	assert.NotContains(routes.concrete, "/api/v1/grouped", "a grouped path is not mounted bare")
	assert.NotContains(routes.concrete, "/api/v1/v2/ping", "an ungrouped path is not mounted under the group")
	assert.Contains(routes.concrete, "/api/v1/raw/{id}")
	assert.NotContains(routes.concrete, "/api/v1/v2/raw/{id}")
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
	sites := prog.constructionSites()
	require.Len(t, sites, 1)
	assert.IsType(t, &ast.CallExpr{}, sites[0].call)
}

func TestRunRejectsUnknownRule(t *testing.T) {
	t.Parallel()
	_, err := Run(t.Context(), Options{Disabled: []string{"bogus"}})
	require.ErrorContains(t, err, `unknown rule "bogus"`)
}
