package humacheck

import (
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/packages"
)

const (
	humaPath     = "github.com/danielgtaylor/huma/v2"
	jsonV1Path   = "encoding/json"
	jsonV2Path   = "encoding/json/v2"
	jsonTextPath = "encoding/json/jsontext"
)

// program indexes the loaded packages so rules can follow calls into
// functions declared anywhere in the module.
type program struct {
	fset  *token.FileSet
	pkgs  []*packages.Package
	funcs map[*types.Func]*funcDecl
}

type funcDecl struct {
	pkg  *packages.Package
	decl *ast.FuncDecl
}

func newProgram(pkgs []*packages.Package) *program {
	p := &program{funcs: make(map[*types.Func]*funcDecl)}
	if len(pkgs) > 0 {
		p.fset = pkgs[0].Fset
	}
	for _, pkg := range pkgs {
		if pkg.TypesInfo == nil {
			continue
		}
		p.pkgs = append(p.pkgs, pkg)
		for _, file := range pkg.Syntax {
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				if obj, ok := pkg.TypesInfo.Defs[fn.Name].(*types.Func); ok {
					p.funcs[obj] = &funcDecl{pkg: pkg, decl: fn}
				}
			}
		}
	}
	return p
}

// eachFunc visits every function declaration with a body in analyzable
// (non-test, non-generated) files.
func (p *program) eachFunc(visit func(pkg *packages.Package, file *ast.File, fn *ast.FuncDecl)) {
	for _, pkg := range p.pkgs {
		for _, file := range pkg.Syntax {
			if p.skipFile(file) {
				continue
			}
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
					visit(pkg, file, fn)
				}
			}
		}
	}
}

// skipFile reports whether a file is a test or generated file. Rules only
// judge hand-written production code.
func (p *program) skipFile(file *ast.File) bool {
	name := p.fset.Position(file.Pos()).Filename
	if strings.HasSuffix(name, "_test.go") || ast.IsGenerated(file) {
		return true
	}
	return strings.Contains(strings.ReplaceAll(name, "\\", "/"), "/generated/")
}

// calleeFunc resolves the function object a call targets, unwrapping
// parentheses and generic instantiation. Nil for dynamic calls and
// conversions.
func calleeFunc(info *types.Info, call *ast.CallExpr) *types.Func {
	var ident *ast.Ident
	switch fun := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		ident = fun
	case *ast.SelectorExpr:
		ident = fun.Sel
	case *ast.IndexExpr:
		return calleeFunc(info, &ast.CallExpr{Fun: fun.X})
	case *ast.IndexListExpr:
		return calleeFunc(info, &ast.CallExpr{Fun: fun.X})
	default:
		return nil
	}
	fn, _ := info.Uses[ident].(*types.Func)
	return fn
}

func pkgPathOf(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Path()
}

func isHumaPkgPath(path string) bool {
	return path == humaPath || strings.HasPrefix(path, humaPath+"/")
}

// isHumaNamed reports whether t is the named type huma.<name> (pointers are
// looked through).
func isHumaNamed(t types.Type, name string) bool {
	if t == nil {
		return false
	}
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Name() == name && pkgPathOf(obj) == humaPath
}

// constString returns the constant string value of expr, if it has one.
func constString(info *types.Info, expr ast.Expr) (string, bool) {
	tv, ok := info.Types[expr]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// rootIdent returns the leftmost identifier of a selector chain such as
// s.cfg.Formats, or the identifier itself.
func rootIdent(expr ast.Expr) *ast.Ident {
	for {
		switch e := ast.Unparen(expr).(type) {
		case *ast.Ident:
			return e
		case *ast.SelectorExpr:
			expr = e.X
		case *ast.IndexExpr:
			expr = e.X
		case *ast.StarExpr:
			expr = e.X
		case *ast.UnaryExpr:
			expr = e.X
		default:
			return nil
		}
	}
}

// selectorOf returns pkg-qualified selector parts for expressions like
// huma.DefaultFormats: the package object and the selected object.
func selectorOf(info *types.Info, expr ast.Expr) (pkgPath string, obj types.Object) {
	sel, ok := ast.Unparen(expr).(*ast.SelectorExpr)
	if !ok {
		return "", nil
	}
	obj = info.Uses[sel.Sel]
	if obj == nil {
		return "", nil
	}
	return pkgPathOf(obj), obj
}

// assignments yields every (lhs, rhs) pair from assignment statements and
// value specs in body, including multi-value forms where counts line up.
func assignments(body ast.Node, visit func(lhs, rhs ast.Expr)) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.AssignStmt:
			if len(node.Lhs) == len(node.Rhs) {
				for i := range node.Lhs {
					visit(node.Lhs[i], node.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if len(node.Names) == len(node.Values) {
				for i := range node.Names {
					visit(node.Names[i], node.Values[i])
				}
			}
		}
		return true
	})
}

// paramObjects returns the parameter variables of fn in declaration order.
func paramObjects(info *types.Info, fn *ast.FuncDecl) []*types.Var {
	var params []*types.Var
	if fn.Type.Params == nil {
		return nil
	}
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			params = append(params, nil)
			continue
		}
		for _, name := range field.Names {
			v, _ := info.Defs[name].(*types.Var)
			params = append(params, v)
		}
	}
	return params
}

// resultObjects returns the named result variables of fn, nil entries for
// unnamed results.
func resultObjects(info *types.Info, fn *ast.FuncDecl) []*types.Var {
	if fn.Type.Results == nil {
		return nil
	}
	var results []*types.Var
	for _, field := range fn.Type.Results.List {
		if len(field.Names) == 0 {
			results = append(results, nil)
			continue
		}
		for _, name := range field.Names {
			v, _ := info.Defs[name].(*types.Var)
			results = append(results, v)
		}
	}
	return results
}
