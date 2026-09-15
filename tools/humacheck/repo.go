package humacheck

import (
	"bytes"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const (
	specJSONMessage    = "OpenAPI document is stored as JSON; commit the Huma OpenAPI document as YAML only (YAML is what agents and reviewers read, and a JSON copy drifts from it)"
	specMissingMessage = "no OpenAPI YAML document is committed for this module; generate the Huma OpenAPI document and commit it as YAML so agents and reviewers can read the contract"
	generatorMissing   = "no standard OpenAPI client generator is configured; use orval for TypeScript clients and github.com/doordash-oss/oapi-codegen-dd/v3 for Go clients"
	maxScanBytes       = 4 << 20
	maxSpecBytes       = 64 << 20
)

var (
	yamlSpecPattern = regexp.MustCompile(`(?m)^openapi:\s*['"]?3\.`)
	jsonSpecPattern = regexp.MustCompile(`"openapi"\s*:\s*"3\.`)

	// A generator declaration is the generator's name as a whole token: a
	// Go module path, a package.json dependency key, or a command word in a
	// Makefile, script, or workflow.
	standardGenerators = []*regexp.Regexp{
		wordPattern("github.com/doordash-oss/oapi-codegen-dd/v3"),
		wordPattern("orval"),
	}
	// Generators that work but fragment the toolchain; each finding names
	// the standard replacement.
	nonstandardGenerators = []generatorRule{
		{wordPattern("github.com/oapi-codegen/oapi-codegen/v2"), "oapi-codegen v2 is not the standard Go generator; migrate to github.com/doordash-oss/oapi-codegen-dd/v3"},
		{wordPattern("github.com/ogen-go/ogen"), "ogen is not the standard Go generator; migrate to github.com/doordash-oss/oapi-codegen-dd/v3"},
		{wordPattern("openapi-typescript"), "openapi-typescript is not the standard TypeScript generator; migrate to orval"},
		{wordPattern("openapi-fetch"), "openapi-fetch is not the standard TypeScript client; migrate to orval"},
		{wordPattern("@hey-api/openapi-ts"), "@hey-api/openapi-ts is not the standard TypeScript generator; migrate to orval"},
	}
	// Generators that must not be used, with the reason shown in diagnostics.
	bannedGenerators = []generatorRule{
		{wordPattern("github.com/deepmap/oapi-codegen"), "github.com/deepmap/oapi-codegen is the unmaintained v1 import path; use github.com/doordash-oss/oapi-codegen-dd/v3"},
		{wordPattern("github.com/go-swagger/go-swagger"), "go-swagger targets Swagger 2.0 and cannot consume Huma's OpenAPI 3 output; use github.com/doordash-oss/oapi-codegen-dd/v3"},
		{wordPattern("openapi-generator-cli"), "openapi-generator produces hand-maintenance-heavy clients; use orval"},
		{wordPattern("openapi-generator"), "openapi-generator produces hand-maintenance-heavy clients; use orval"},
		{wordPattern("swagger-codegen"), "swagger-codegen is unsupported for OpenAPI 3.1; use orval"},
		{wordPattern("swagger-typescript-api"), "swagger-typescript-api is not a supported generator; use orval"},
	}
)

type generatorRule struct {
	pattern *regexp.Regexp
	message string
}

// wordPattern builds a matcher for name as a whole word: not embedded in a longer
// identifier, module path, or package name.
func wordPattern(name string) *regexp.Regexp {
	return regexp.MustCompile(`(?:^|[^\w.-])` + regexp.QuoteMeta(name) + `(?:[^\w.-]|$)`)
}

// excludedDir reports directories whose contents never count as repository
// artifacts.
func excludedDir(p string) bool {
	for segment := range strings.SplitSeq(path.Dir(p), "/") {
		switch segment {
		case ".git", "node_modules", "vendor", "testdata", "dist", "build", ".worktrees":
			return true
		}
	}
	return false
}

func readHead(repo fs.FS, name string, limit int64) ([]byte, error) {
	f, err := repo.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, limit))
}

// underDir reports whether name lies inside dir ("." means everywhere).
func underDir(name, dir string) bool {
	return dir == "." || dir == "" || name == dir || strings.HasPrefix(name, dir+"/")
}

// checkSpecs enforces that OpenAPI documents anywhere in the repository are
// committed as YAML, and that a module building a Huma API commits at least
// one inside its own directory. A sibling module's contract does not count.
func checkSpecs(repo fs.FS, tracked []string, hasAPI bool, goModPath string) []Diagnostic {
	var diags []Diagnostic
	moduleDir := path.Dir(goModPath)
	yamlSpecs := 0
	for _, name := range tracked {
		if excludedDir(name) {
			continue
		}
		ext := strings.ToLower(path.Ext(name))
		if ext != ".json" && ext != ".yaml" && ext != ".yml" {
			continue
		}
		// Huma emits keys alphabetically, so "openapi" follows the whole
		// components section; the entire document must be scanned.
		content, err := readHead(repo, name, maxSpecBytes)
		if err != nil {
			continue
		}
		switch ext {
		case ".json":
			if jsonSpecPattern.Match(content) {
				diags = append(diags, Diagnostic{Path: name, Line: 1, Column: 1, Rule: RuleSpec, Message: specJSONMessage})
			}
		default:
			if yamlSpecPattern.Match(content) && underDir(name, moduleDir) {
				yamlSpecs++
			}
		}
	}
	if hasAPI && yamlSpecs == 0 {
		diags = append(diags, Diagnostic{Path: goModPath, Line: 1, Column: 1, Rule: RuleSpec, Message: specMissingMessage})
	}
	return diags
}

// generatorDeclarationFile reports files that can name a code generator:
// module and package manifests, build scripts, generator configs, and
// workflow or script sources.
func generatorDeclarationFile(name string) bool {
	base := path.Base(name)
	switch base {
	case "go.mod", "package.json", "Makefile", "GNUmakefile", "makefile", "Justfile", "justfile", "Taskfile.yml", "Taskfile.yaml":
		return true
	}
	if strings.HasPrefix(base, "Makefile.") || strings.HasPrefix(base, "oapi-codegen") || strings.HasPrefix(base, "orval.config") || strings.HasPrefix(base, "openapi-ts.config") || strings.HasPrefix(base, "ogen") {
		return true
	}
	switch path.Ext(base) {
	case ".mk", ".sh":
		return true
	}
	if strings.HasPrefix(name, ".github/workflows/") {
		return true
	}
	for segment := range strings.SplitSeq(path.Dir(name), "/") {
		if segment == "scripts" || segment == "script" || segment == "tools" {
			switch path.Ext(base) {
			case ".mjs", ".cjs", ".js", ".ts", ".yaml", ".yml", ".toml":
				return true
			}
		}
	}
	return false
}

// declarationLines returns the lines of a file that can declare a
// generator, keyed by line number. Comment lines are dropped everywhere; Go
// source contributes only go:generate directives; go.mod drops transitive
// requirements marked indirect, which are not a choice the repository made.
func declarationLines(name string, content []byte) map[int]string {
	base := path.Base(name)
	isGo := strings.HasSuffix(base, ".go")
	isGoMod := base == "go.mod"
	lines := map[int]string{}
	lineNo := 0
	for raw := range bytes.SplitSeq(content, []byte("\n")) {
		lineNo++
		line := strings.TrimSpace(string(raw))
		switch {
		case isGo:
			if !strings.HasPrefix(line, "//go:generate") {
				continue
			}
		case strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "*"):
			continue
		case isGoMod && strings.Contains(line, "// indirect"):
			continue
		}
		lines[lineNo] = line
	}
	return lines
}

// checkGenerators flags banned and nonstandard generators where they are
// declared and, for modules with a Huma API, the absence of a standard one.
func checkGenerators(repo fs.FS, tracked []string, hasAPI bool, goModPath string) []Diagnostic {
	var diags []Diagnostic
	standard := false
	rules := append(append([]generatorRule{}, bannedGenerators...), nonstandardGenerators...)
	for _, name := range tracked {
		if excludedDir(name) {
			continue
		}
		isGo := strings.HasSuffix(name, ".go") && !strings.HasSuffix(name, "_test.go")
		if !isGo && !generatorDeclarationFile(name) {
			continue
		}
		content, err := readHead(repo, name, maxScanBytes)
		if err != nil {
			continue
		}
		reported := map[string]bool{}
		for lineNo, line := range declarationLines(name, content) {
			for _, pattern := range standardGenerators {
				if pattern.MatchString(line) {
					standard = true
				}
			}
			for _, rule := range rules {
				if !rule.pattern.MatchString(line) {
					continue
				}
				key := strconv.Itoa(lineNo) + ":" + rule.message
				if reported[key] {
					continue
				}
				reported[key] = true
				diags = append(diags, Diagnostic{Path: name, Line: lineNo, Column: 1, Rule: RuleGenerator, Message: rule.message})
			}
		}
	}
	if hasAPI && !standard {
		diags = append(diags, Diagnostic{Path: goModPath, Line: 1, Column: 1, Rule: RuleGenerator, Message: generatorMissing})
	}
	return diags
}

func frontendSourceFile(name string) bool {
	if excludedDir(name) {
		return false
	}
	base := path.Base(name)
	switch path.Ext(base) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".svelte", ".vue":
	default:
		return false
	}
	if strings.HasSuffix(base, ".d.ts") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.Contains(base, ".stories.") || strings.Contains(base, ".browser.") || strings.Contains(base, "e2e") {
		return false
	}
	for segment := range strings.SplitSeq(path.Dir(name), "/") {
		switch segment {
		case "generated", "__tests__", "test", "tests", "e2e", "mocks", "__mocks__", "fixtures":
			return false
		}
	}
	return true
}

// checkFrontend scans hand-written browser code for request calls whose
// URL argument names one of the module's routes.
func checkFrontend(repo fs.FS, tracked []string, routes *routeSet) []Diagnostic {
	if routes == nil || len(routes.concrete) == 0 {
		return nil
	}
	var diags []Diagnostic
	for _, name := range tracked {
		if !frontendSourceFile(name) {
			continue
		}
		content, err := readHead(repo, name, maxScanBytes)
		if err != nil {
			continue
		}
		diags = append(diags, scanFrontendSource(name, content, routes)...)
	}
	return diags
}

// frontendCallPattern matches the opening of a request call: fetch(...),
// new Request(...), axios(...) or axios.get(...), and ky(...) or ky.get(...).
var frontendCallPattern = regexp.MustCompile(`\b(?:fetch|axios(?:\.\w+)?|ky(?:\.\w+)?)\s*\(|\bnew\s+Request\s*\(`)

// scanFrontendSource reports route literals that appear in the first
// argument of a request call. The argument is delimited by balanced
// brackets with string and template literals respected, so options objects
// and unrelated statements are not inspected.
func scanFrontendSource(name string, content []byte, routes *routeSet) []Diagnostic {
	var diags []Diagnostic
	src := string(content)
	for _, loc := range frontendCallPattern.FindAllStringIndex(src, -1) {
		lineStart := strings.LastIndexByte(src[:loc[0]], '\n') + 1
		if lead := strings.TrimSpace(src[lineStart:loc[0]]); strings.HasPrefix(lead, "//") || strings.HasPrefix(lead, "*") || strings.HasPrefix(lead, "import") {
			continue
		}
		argStart := loc[1]
		argEnd, ok := firstArgumentEnd(src, argStart)
		if !ok {
			continue
		}
		for _, lit := range stringLiterals(src[argStart:argEnd]) {
			route, ok := routes.match(lit.text)
			if !ok {
				continue
			}
			offset := argStart + lit.offset
			line := strings.Count(src[:offset], "\n") + 1
			column := offset - (strings.LastIndexByte(src[:offset], '\n') + 1) + 1
			diags = append(diags, Diagnostic{
				Path: name, Line: line, Column: column, Rule: RuleFrontend,
				Message: formatClientMessage(route),
			})
		}
	}
	return diags
}

// firstArgumentEnd returns the offset just past the first argument that
// starts at start: the top-level comma or the closing parenthesis.
func firstArgumentEnd(src string, start int) (int, bool) {
	depth := 0
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '"', '\'', '`':
			end := literalEnd(src, i)
			if end < 0 {
				return 0, false
			}
			i = end
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			if depth == 0 {
				return i, true
			}
			depth--
		case ',':
			if depth == 0 {
				return i, true
			}
		}
	}
	return 0, false
}

// literalEnd returns the index of the closing quote of the string literal
// opening at src[start], honoring escapes, or -1 if unterminated.
func literalEnd(src string, start int) int {
	quote := src[start]
	for i := start + 1; i < len(src); i++ {
		switch src[i] {
		case '\\':
			i++
		case quote:
			return i
		}
	}
	return -1
}

type literal struct {
	offset int    // offset of the opening quote within the scanned text
	text   string // contents with template holes replaced by %s
}

var templateHolePattern = regexp.MustCompile(`\$\{[^}]*\}`)

// stringLiterals returns every string or template literal in src.
func stringLiterals(src string) []literal {
	var lits []literal
	for i := 0; i < len(src); i++ {
		switch src[i] {
		case '"', '\'', '`':
			end := literalEnd(src, i)
			if end < 0 {
				return lits
			}
			text := src[i+1 : end]
			if src[i] == '`' {
				text = templateHolePattern.ReplaceAllString(text, "%s")
			}
			lits = append(lits, literal{offset: i, text: text})
			i = end
		}
	}
	return lits
}
