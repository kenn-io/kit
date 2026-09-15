// Package humacheck reports Huma API usage that drifts from the shared
// contract: APIs must serialize with encoding/json/v2, the generated OpenAPI
// document must be committed as YAML, clients must come from the standard
// generator, and nobody may hand-roll HTTP calls against the module's own
// routes.
package humacheck

import (
	"context"
	"errors"
	"fmt"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Rule names accepted by Options.Disabled.
const (
	RuleJSONV2    = "jsonv2"
	RuleSpec      = "spec"
	RuleGenerator = "generator"
	RuleClient    = "client"
	RuleFrontend  = "frontend"
)

// Rules lists every rule name in output order.
var Rules = []string{RuleJSONV2, RuleSpec, RuleGenerator, RuleClient, RuleFrontend}

// Options configures a Run.
type Options struct {
	// Dir is the working directory for package loading and path display.
	// Empty means the process working directory.
	Dir string
	// Patterns are go/packages patterns; empty means "./...".
	Patterns []string
	// BuildTags are extra build tags passed to the loader.
	BuildTags []string
	// Disabled lists rule names that must not run.
	Disabled []string
}

// Diagnostic is one finding.
type Diagnostic struct {
	Path    string
	Line    int
	Column  int
	Rule    string
	Message string
}

// String renders the diagnostic in the conventional path:line:col form.
func (d Diagnostic) String() string {
	return fmt.Sprintf("%s:%d:%d: %s (%s)", d.Path, d.Line, d.Column, d.Message, d.Rule)
}

// Compare orders diagnostics by path, line, column, then rule.
func (d Diagnostic) Compare(other Diagnostic) int {
	if c := strings.Compare(d.Path, other.Path); c != 0 {
		return c
	}
	if d.Line != other.Line {
		return d.Line - other.Line
	}
	if d.Column != other.Column {
		return d.Column - other.Column
	}
	return strings.Compare(d.Rule, other.Rule)
}

// Run loads the packages named by opts and returns every finding sorted by
// position. Paths are relative to opts.Dir when possible.
func Run(ctx context.Context, opts Options) ([]Diagnostic, error) {
	dir := opts.Dir
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		dir = resolved
	}
	for _, rule := range opts.Disabled {
		if !slices.Contains(Rules, rule) {
			return nil, fmt.Errorf("unknown rule %q; known rules: %s", rule, strings.Join(Rules, ", "))
		}
	}
	patterns := opts.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	cfg := &packages.Config{
		Context: ctx,
		Dir:     dir,
		Mode:    loadMode,
		Tests:   false,
	}
	if len(opts.BuildTags) > 0 {
		cfg.BuildFlags = []string{"-tags=" + strings.Join(opts.BuildTags, ",")}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if err := packageErrors(pkgs); err != nil {
		return nil, err
	}

	layout, err := resolveLayout(ctx, dir, pkgs)
	if err != nil {
		return nil, err
	}
	repo, tracked, closeRepo, err := layout.open(ctx)
	if err != nil {
		return nil, err
	}
	defer closeRepo()

	diags := check(pkgs, repo, tracked, layout.goMod, opts.Disabled)
	for i := range diags {
		diags[i].Path = displayPath(dir, layout.root, diags[i].Path)
	}
	slices.SortFunc(diags, Diagnostic.Compare)
	return diags, nil
}

const loadMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
	packages.NeedImports | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedSyntax |
	packages.NeedModule | packages.NeedDeps

// check runs every enabled rule over already-loaded packages. Repository rules
// read tracked files (repo-root-relative, forward slashes) through repo.
// goModPath is the repo-relative go.mod that anchors module-wide findings and
// scopes the spec-presence rule.
func check(pkgs []*packages.Package, repo fs.FS, tracked []string, goModPath string, disabled []string) []Diagnostic {
	enabled := func(rule string) bool { return !slices.Contains(disabled, rule) }
	prog := newProgram(pkgs)
	jsonv2 := newJSONV2Checker(prog)

	var diags []Diagnostic
	sites := prog.constructionSites(jsonv2)
	if enabled(RuleJSONV2) {
		diags = append(diags, jsonv2.check(sites)...)
	}
	routes := collectRoutes(prog)
	if enabled(RuleClient) {
		diags = append(diags, checkClients(prog, routes)...)
	}
	hasAPI := len(sites) > 0
	if enabled(RuleSpec) {
		diags = append(diags, checkSpecs(repo, tracked, hasAPI, goModPath)...)
	}
	if enabled(RuleGenerator) {
		diags = append(diags, checkGenerators(repo, tracked, hasAPI, goModPath)...)
	}
	if enabled(RuleFrontend) {
		diags = append(diags, checkFrontend(repo, tracked, routes)...)
	}
	return diags
}

func packageErrors(pkgs []*packages.Package) error {
	var errs []error
	packages.Visit(pkgs, nil, func(pkg *packages.Package) {
		for _, e := range pkg.Errors {
			errs = append(errs, errors.New(e.Error()))
		}
	})
	if len(errs) == 0 {
		return nil
	}
	if len(errs) > 20 {
		errs = append(errs[:20], fmt.Errorf("%d more errors", len(errs)-20))
	}
	return fmt.Errorf("packages failed to load: %w", errors.Join(errs...))
}

// layout locates the repository and module the checked packages belong to.
type layout struct {
	root      string // repository root, or the module directory outside Git
	moduleDir string
	goMod     string // repo-relative go.mod path
	inGit     bool
}

func resolveLayout(ctx context.Context, dir string, pkgs []*packages.Package) (layout, error) {
	moduleDir := dir
	for _, pkg := range pkgs {
		if pkg.Module != nil && pkg.Module.Main && pkg.Module.Dir != "" {
			moduleDir = pkg.Module.Dir
			break
		}
	}
	if resolved, err := filepath.EvalSymlinks(moduleDir); err == nil {
		moduleDir = resolved
	}
	root, inGit, err := gitTopLevel(ctx, moduleDir)
	if err != nil {
		return layout{}, err
	}
	if !inGit {
		root = moduleDir
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	l := layout{root: root, moduleDir: moduleDir, inGit: inGit, goMod: "go.mod"}
	if rel, err := filepath.Rel(root, filepath.Join(moduleDir, "go.mod")); err == nil && !strings.HasPrefix(rel, "..") {
		l.goMod = filepath.ToSlash(rel)
	}
	return l, nil
}

// open returns the repository contents and file list. Inside Git both come
// from the index, so a pre-commit run judges what will be committed; outside
// Git the directory tree is used.
func (l layout) open(ctx context.Context) (fs.FS, []string, func(), error) {
	if !l.inGit {
		files, err := walkFiles(l.root)
		if err != nil {
			return nil, nil, nil, err
		}
		return os.DirFS(l.root), files, func() {}, nil
	}
	tracked, err := trackedFiles(ctx, l.root)
	if err != nil {
		return nil, nil, nil, err
	}
	index, err := newIndexFS(ctx, l.root)
	if err != nil {
		return nil, nil, nil, err
	}
	return index, tracked, func() { _ = index.Close() }, nil
}

// displayPath makes p relative to dir. Repo-relative paths (no separator
// prefix) are first anchored at repoRoot.
func displayPath(dir, repoRoot, p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(repoRoot, filepath.FromSlash(p))
	}
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return p
	}
	return rel
}

func positionOf(fset *token.FileSet, pos token.Pos) (string, int, int) {
	position := fset.Position(pos)
	return position.Filename, position.Line, position.Column
}
