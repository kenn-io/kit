package humacheck

import (
	"bufio"
	"bytes"
	"io"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

const (
	specJSONMessage    = "OpenAPI document is stored as JSON; commit the Huma OpenAPI document as YAML only (YAML is what agents and reviewers read, and a JSON copy drifts from it)"
	specMissingMessage = "no OpenAPI YAML document is committed; generate the Huma OpenAPI document and commit it as YAML so agents and reviewers can read the contract"
	generatorMissing   = "no supported OpenAPI client generator is configured (expected one of: " + supportedGeneratorList + ")"
	maxScanBytes       = 4 << 20
	maxSpecBytes       = 64 << 20
)

// supportedGeneratorList documents the allowlist in diagnostics.
const supportedGeneratorList = "oapi-codegen-dd/v3, oapi-codegen/v2, ogen, orval, openapi-typescript, @hey-api/openapi-ts"

var (
	yamlSpecPattern = regexp.MustCompile(`(?m)^openapi:\s*['"]?3\.`)
	jsonSpecPattern = regexp.MustCompile(`"openapi"\s*:\s*"3\.`)

	// Generators recognized as acceptable. Matched as substrings of the files
	// that declare toolchains.
	supportedGenerators = []string{
		"github.com/doordash-oss/oapi-codegen-dd/v3",
		"github.com/oapi-codegen/oapi-codegen/v2",
		"github.com/ogen-go/ogen",
		`"orval"`,
		`"openapi-typescript"`,
		`"@hey-api/openapi-ts"`,
	}
	// Generators that must not be used, with the reason shown in diagnostics.
	bannedGenerators = []struct{ needle, message string }{
		{"github.com/deepmap/oapi-codegen", "github.com/deepmap/oapi-codegen is the unmaintained v1 import path; use github.com/doordash-oss/oapi-codegen-dd/v3 or github.com/oapi-codegen/oapi-codegen/v2"},
		{"github.com/go-swagger/go-swagger", "go-swagger targets Swagger 2.0 and cannot consume Huma's OpenAPI 3 output; use oapi-codegen"},
		{"openapi-generator-cli", "openapi-generator produces hand-maintenance-heavy clients; use orval, openapi-typescript, or @hey-api/openapi-ts"},
		{"openapi-generator", "openapi-generator produces hand-maintenance-heavy clients; use orval, openapi-typescript, or @hey-api/openapi-ts"},
		{"swagger-codegen", "swagger-codegen is unsupported for OpenAPI 3.1; use orval, openapi-typescript, or @hey-api/openapi-ts"},
		{"swagger-typescript-api", "swagger-typescript-api is not a supported generator; use orval, openapi-typescript, or @hey-api/openapi-ts"},
	}
)

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

// checkSpecs enforces that OpenAPI documents are committed as YAML, and that
// a module with a Huma API commits at least one.
func checkSpecs(repo fs.FS, tracked []string, hasAPI bool, goModPath string) []Diagnostic {
	var diags []Diagnostic
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
			if yamlSpecPattern.Match(content) {
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
// module and package manifests, build scripts, generate directives, and
// generator configs.
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
	case ".go":
		return strings.HasPrefix(base, "gen") || strings.Contains(base, "generate")
	}
	dir := path.Dir(name)
	if strings.HasPrefix(name, ".github/workflows/") {
		return true
	}
	for segment := range strings.SplitSeq(dir, "/") {
		if segment == "scripts" || segment == "script" || segment == "tools" {
			switch path.Ext(base) {
			case ".mjs", ".cjs", ".js", ".ts", ".yaml", ".yml", ".toml":
				return true
			}
		}
	}
	return false
}

// checkGenerators flags banned generators and, for modules with a Huma API,
// the absence of any supported one.
func checkGenerators(repo fs.FS, tracked []string, hasAPI bool, goModPath string) []Diagnostic {
	var diags []Diagnostic
	supported := false
	for _, name := range tracked {
		if excludedDir(name) || !generatorDeclarationFile(name) {
			continue
		}
		content, err := readHead(repo, name, maxScanBytes)
		if err != nil {
			continue
		}
		for _, needle := range supportedGenerators {
			if bytes.Contains(content, []byte(needle)) {
				supported = true
				break
			}
		}
		reported := map[int]bool{}
		for _, banned := range bannedGenerators {
			line := lineContaining(content, banned.needle)
			if line == 0 || reported[line] {
				continue
			}
			reported[line] = true
			diags = append(diags, Diagnostic{Path: name, Line: line, Column: 1, Rule: RuleGenerator, Message: banned.message})
		}
	}
	if hasAPI && !supported {
		diags = append(diags, Diagnostic{Path: goModPath, Line: 1, Column: 1, Rule: RuleGenerator, Message: generatorMissing})
	}
	return diags
}

func lineContaining(content []byte, needle string) int {
	before, _, found := bytes.Cut(content, []byte(needle))
	if !found {
		return 0
	}
	return bytes.Count(before, []byte("\n")) + 1
}

var (
	frontendCallPattern    = regexp.MustCompile(`\bfetch\s*\(|new\s+Request\s*\(|\baxios\b|\bky\s*[.(]`)
	frontendLiteralPattern = regexp.MustCompile("`[^`]*`|\"(?:[^\"\\\\]|\\\\.)*\"|'(?:[^'\\\\]|\\\\.)*'")
	templateHolePattern    = regexp.MustCompile(`\$\{[^}]*\}`)
)

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

// checkFrontend scans hand-written browser code for fetch-style calls whose
// URL literal names one of the module's routes.
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

// scanFrontendSource reports route literals that sit on, or within three
// lines after, a request-building call.
func scanFrontendSource(name string, content []byte, routes *routeSet) []Diagnostic {
	var diags []Diagnostic
	scanner := bufio.NewScanner(bytes.NewReader(content))
	scanner.Buffer(make([]byte, 0, 64<<10), maxScanBytes)
	lastCall := -10
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		if frontendCallPattern.MatchString(line) {
			lastCall = lineNo
		}
		if lineNo-lastCall > 3 {
			continue
		}
		for _, loc := range frontendLiteralPattern.FindAllStringIndex(line, -1) {
			raw := line[loc[0]+1 : loc[1]-1]
			literal := templateHolePattern.ReplaceAllString(raw, "%s")
			route, ok := routes.match(literal)
			if !ok {
				continue
			}
			diags = append(diags, Diagnostic{
				Path: name, Line: lineNo, Column: loc[0] + 1, Rule: RuleFrontend,
				Message: formatClientMessage(route),
			})
		}
	}
	return diags
}
