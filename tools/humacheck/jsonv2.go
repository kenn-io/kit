package humacheck

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	jsonV1ImportMessage  = "encoding/json (v1) is imported; use encoding/json/v2 and encoding/json/jsontext so Huma payloads get v2 semantics (nil slices encode as [], not null)"
	jsonV2MissingMessage = "this module builds a Huma API but never installs an encoding/json/v2 huma.Format; set config.Formats[\"application/json\"] (or huma.DefaultJSONFormat) to a huma.Format whose Marshal and Unmarshal use encoding/json/v2"
)

// verdict is a three-valued answer for "does this use JSON v2".
type verdict int

const (
	unknown verdict = iota
	yes
	no
)

const maxDepth = 6

// constructionSite is a call that hands a huma.Config to Huma or builds a
// Huma adapter; its presence means the module serves a Huma API.
type constructionSite struct {
	pkg  *packages.Package
	call *ast.CallExpr
}

// constructionSites finds every production call into a Huma package that
// takes a huma.Config or returns a huma.API or huma.Adapter, in function
// bodies and package-level initializers alike.
func (p *program) constructionSites() []constructionSite {
	var sites []constructionSite
	p.eachRoot(func(pkg *packages.Package, root ast.Node, _ []*types.Var) {
		ast.Inspect(root, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeFunc(pkg.TypesInfo, call)
			if callee == nil || !isHumaPkgPath(pkgPathOf(callee)) {
				return true
			}
			if takesConfig(callee) || constructsAPI(callee) {
				sites = append(sites, constructionSite{pkg: pkg, call: call})
			}
			return true
		})
	})
	return sites
}

func takesConfig(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	for param := range sig.Params().Variables() {
		if isHumaNamed(param.Type(), "Config") {
			return true
		}
	}
	return false
}

// checkJSONV2 enforces JSON v2 with two module-wide checks and no value
// tracing: no production file in the module imports encoding/json (v1),
// and a module that builds a Huma API installs at least one huma.Format
// backed by encoding/json/v2 somewhere. Huma's own defaults encode with v1
// inside the huma package, so a module with no v1 import still needs the
// install.
//
// The import ban reads every tracked Go file under the module directory
// rather than the loaded packages, so files excluded by build tags or
// platform suffixes are covered too.
func checkJSONV2(p *program, repo fs.FS, tracked []string, hasAPI bool, goModPath string) ([]Diagnostic, error) {
	var diags []Diagnostic
	moduleDir := path.Dir(goModPath)
	for _, name := range tracked {
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") || !underDir(name, moduleDir) || excludedDir(name) || strings.Contains("/"+name, "/generated/") {
			continue
		}
		content, err := readTracked(repo, name)
		if err != nil {
			return nil, err
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, content, parser.ImportsOnly|parser.ParseComments)
		if err != nil || ast.IsGenerated(file) {
			// A file that does not parse is reported by the Go loader when it
			// is part of the build; otherwise it cannot import anything.
			continue
		}
		for _, imp := range file.Imports {
			if importPath(imp) == jsonV1Path {
				pos := fset.Position(imp.Pos())
				diags = append(diags, Diagnostic{Path: name, Line: pos.Line, Column: pos.Column, Rule: RuleJSONV2, Message: jsonV1ImportMessage})
			}
		}
	}
	if hasAPI && !newFormatFinder(p).installsV2() {
		diags = append(diags, Diagnostic{Path: goModPath, Line: 1, Column: 1, Rule: RuleJSONV2, Message: jsonV2MissingMessage})
	}
	return diags, nil
}

func importPath(imp *ast.ImportSpec) string {
	path, err := strconv.Unquote(imp.Path.Value)
	if err != nil {
		return ""
	}
	return path
}

// formatFinder looks for one huma.Format install backed by JSON v2 anywhere
// in the module. It is an existence check: which API the format reaches is
// not tracked.
type formatFinder struct {
	p           *program
	formatFuncs map[*types.Func]verdict
	// defaultOverridden is set when huma.DefaultJSONFormat is assigned a v2
	// format. That alone installs nothing: huma.DefaultFormats copied the
	// old value at package init and huma.DefaultConfig reads the map, so the
	// variable only counts once it is written into a Formats map.
	defaultOverridden bool
}

func newFormatFinder(p *program) *formatFinder {
	return &formatFinder{p: p, formatFuncs: make(map[*types.Func]verdict)}
}

// installsV2 reports whether any application/json map install (a Formats or
// huma.DefaultFormats index assignment or a map literal entry) carries a v2
// format. A huma.DefaultJSONFormat assignment only makes that variable v2;
// it must still be written into a map to count.
func (f *formatFinder) installsV2() bool {
	// Pass one: does anything replace Huma's default JSON format with v2?
	f.eachInstall(func(pkg *packages.Package, isDefault bool, value ast.Expr) bool {
		if isDefault && f.formatExpr(pkg, value, 0) == yes {
			f.defaultOverridden = true
			return false
		}
		return true
	})
	found := false
	f.eachInstall(func(pkg *packages.Package, isDefault bool, value ast.Expr) bool {
		if !isDefault && f.formatExpr(pkg, value, 0) == yes {
			found = true
			return false
		}
		return true
	})
	return found
}

// eachInstall visits every expression assigned as an application/json
// format in production code, including package-level initializers. visit
// returns false to stop.
func (f *formatFinder) eachInstall(visit func(pkg *packages.Package, isDefault bool, value ast.Expr) bool) {
	for _, pkg := range f.p.pkgs {
		info := pkg.TypesInfo
		for _, file := range pkg.Syntax {
			if f.p.skipFile(file) {
				continue
			}
			stop := false
			ast.Inspect(file, func(n ast.Node) bool {
				if stop {
					return false
				}
				switch node := n.(type) {
				case *ast.AssignStmt:
					if len(node.Lhs) != len(node.Rhs) {
						return true
					}
					for i := range node.Lhs {
						if isDefault, ok := installTarget(info, node.Lhs[i]); ok && !visit(pkg, isDefault, node.Rhs[i]) {
							stop = true
							return false
						}
					}
				case *ast.CompositeLit:
					if !isFormatMap(info.TypeOf(node)) {
						return true
					}
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := constString(info, kv.Key); ok && key == "application/json" && !visit(pkg, false, kv.Value) {
							stop = true
							return false
						}
					}
				}
				return true
			})
			if stop {
				return
			}
		}
	}
}

// installTarget reports whether lhs is an application/json format slot:
// huma.DefaultJSONFormat (isDefault) or an "application/json" index of a
// map[string]huma.Format such as config.Formats or huma.DefaultFormats.
func installTarget(info *types.Info, lhs ast.Expr) (isDefault bool, ok bool) {
	switch l := ast.Unparen(lhs).(type) {
	case *ast.SelectorExpr:
		if path, obj := selectorOf(info, l); obj != nil && path == humaPath && obj.Name() == "DefaultJSONFormat" {
			return true, true
		}
	case *ast.IndexExpr:
		if !isFormatMap(info.TypeOf(l.X)) {
			return false, false
		}
		if key, ok := constString(info, l.Index); ok && key == "application/json" {
			return false, true
		}
	}
	return false, false
}

func isFormatMap(t types.Type) bool {
	if t == nil {
		return false
	}
	m, ok := t.Underlying().(*types.Map)
	return ok && isHumaNamed(m.Elem(), "Format")
}

// formatExpr judges an expression of type huma.Format: yes only when both
// codecs are proven v2, no only when either is proven v1, else unknown.
func (f *formatFinder) formatExpr(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.CompositeLit:
		if !isHumaNamed(info.TypeOf(e), "Format") {
			return unknown
		}
		marshal, unmarshal := unknown, unknown
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				continue
			}
			switch key.Name {
			case "Marshal":
				marshal = f.funcUsesV2(pkg, kv.Value, 0)
			case "Unmarshal":
				unmarshal = f.funcUsesV2(pkg, kv.Value, 0)
			}
		}
		switch {
		case marshal == no || unmarshal == no:
			return no
		case marshal == yes && unmarshal == yes:
			return yes
		}
		return unknown
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return f.formatExpr(pkg, e.X, depth+1)
		}
	case *ast.SelectorExpr:
		pkgPath, obj := selectorOf(info, e)
		if obj == nil {
			return unknown
		}
		if pkgPath == humaPath && obj.Name() == "DefaultJSONFormat" {
			if f.defaultOverridden {
				return yes
			}
			return no
		}
		// A format defined in another module package, such as
		// codecs.JSONFormat, is judged in its defining package.
		if v, ok := obj.(*types.Var); ok {
			if home := f.p.packageByPath(pkgPath); home != nil {
				return f.varFormat(home, v, depth+1)
			}
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		if home := f.p.packageByPath(pkgPathOf(obj)); home != nil {
			pkg = home
		}
		return f.varFormat(pkg, obj, depth+1)
	}
	return unknown
}

// varFormat judges a variable by the values assigned to it anywhere in its
// package: yes when some assignment is a v2 format and none is v1.
func (f *formatFinder) varFormat(pkg *packages.Package, obj *types.Var, depth int) verdict {
	info := pkg.TypesInfo
	result := unknown
	for _, file := range pkg.Syntax {
		ast.Inspect(file, func(n ast.Node) bool {
			var lhs, rhs []ast.Expr
			switch node := n.(type) {
			case *ast.AssignStmt:
				lhs, rhs = node.Lhs, node.Rhs
			case *ast.ValueSpec:
				for _, name := range node.Names {
					lhs = append(lhs, name)
				}
				rhs = node.Values
			default:
				return true
			}
			if len(lhs) != len(rhs) {
				return true
			}
			for i, l := range lhs {
				ident, ok := ast.Unparen(l).(*ast.Ident)
				if !ok || (info.Defs[ident] != obj && info.Uses[ident] != obj) {
					continue
				}
				switch f.formatExpr(pkg, rhs[i], depth) {
				case no:
					result = no
				case yes:
					if result == unknown {
						result = yes
					}
				}
			}
			return true
		})
	}
	return result
}

// funcUsesV2 judges a Marshal or Unmarshal value: a function literal or a
// reference to a function. It is yes only when the body reaches
// encoding/json/v2 and never calls an encoding/json (v1) codec function.
func (f *formatFinder) funcUsesV2(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.FuncLit:
		return f.bodyUsesV2(pkg, e.Body, depth)
	case *ast.Ident, *ast.SelectorExpr:
		var obj types.Object
		if id, ok := e.(*ast.Ident); ok {
			obj = info.Uses[id]
		} else {
			_, obj = selectorOf(info, e)
		}
		fn, ok := obj.(*types.Func)
		if !ok {
			return unknown
		}
		return f.funcDeclUsesV2(fn, depth)
	}
	return unknown
}

func (f *formatFinder) funcDeclUsesV2(fn *types.Func, depth int) verdict {
	switch path := pkgPathOf(fn); {
	case path == jsonV2Path || path == jsonTextPath:
		return yes
	case path == jsonV1Path && isV1Codec(fn.Name()):
		return no
	}
	if v, ok := f.formatFuncs[fn]; ok {
		return v
	}
	fd, ok := f.p.funcs[fn]
	if !ok {
		return unknown
	}
	f.formatFuncs[fn] = unknown // cycle guard
	v := f.bodyUsesV2(fd.pkg, fd.decl.Body, depth+1)
	f.formatFuncs[fn] = v
	return v
}

// bodyUsesV2 scans calls in body. Local callees are followed so thin
// wrappers such as marshalAPIJSON count.
func (f *formatFinder) bodyUsesV2(pkg *packages.Package, body *ast.BlockStmt, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	sawV2, sawV1 := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn := calleeFunc(pkg.TypesInfo, call)
		if fn == nil {
			return true
		}
		switch path := pkgPathOf(fn); {
		case path == jsonV2Path || path == jsonTextPath:
			sawV2 = true
		case path == jsonV1Path && isV1Codec(fn.Name()):
			sawV1 = true
		default:
			if _, local := f.p.funcs[fn]; local {
				switch f.funcDeclUsesV2(fn, depth+1) {
				case yes:
					sawV2 = true
				case no:
					sawV1 = true
				}
			}
		}
		return true
	})
	switch {
	case sawV1:
		return no
	case sawV2:
		return yes
	}
	return unknown
}

func isV1Codec(name string) bool {
	switch name {
	case "Marshal", "MarshalIndent", "Unmarshal", "NewEncoder", "NewDecoder":
		return true
	}
	return false
}
