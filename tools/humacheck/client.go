package humacheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/packages"
)

// checkClients reports string literals that name one of the module's own
// routes and are handed to code that builds HTTP requests.
func checkClients(p *program, routes *routeSet) []Diagnostic {
	if routes == nil || len(routes.concrete) == 0 {
		return nil
	}
	requesters := p.requesters()
	var diags []Diagnostic
	seen := map[token.Pos]bool{}
	report := func(pkg *packages.Package, expr ast.Expr) {
		literal, ok := constString(pkg.TypesInfo, expr)
		if !ok {
			return
		}
		route, ok := routes.match(literal)
		if !ok || seen[expr.Pos()] {
			return
		}
		seen[expr.Pos()] = true
		path, line, col := positionOf(p.fset, expr.Pos())
		diags = append(diags, Diagnostic{
			Path: path, Line: line, Column: col, Rule: RuleClient,
			Message: formatClientMessage(route),
		})
	}

	p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		info := pkg.TypesInfo
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			callee := calleeFunc(info, call)
			if callee == nil {
				return true
			}
			if idx, ok := urlArgIndex(callee); ok {
				if idx < len(call.Args) {
					eachStringConst(info, call.Args[idx], func(e ast.Expr) { report(pkg, e) })
				}
				return true
			}
			if requesters[callee] {
				for _, arg := range call.Args {
					eachStringConst(info, arg, func(e ast.Expr) { report(pkg, e) })
				}
			}
			return true
		})
	})
	return diags
}

func formatClientMessage(route string) string {
	return "hand-rolled HTTP request to this module's own Huma route " + route + "; call it through the generated API client"
}

// urlArgIndex identifies net/http request builders and the index of their
// URL argument.
func urlArgIndex(fn *types.Func) (int, bool) {
	if pkgPathOf(fn) != "net/http" {
		return 0, false
	}
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return 0, false
	}
	if sig.Recv() != nil {
		if !isNamed(sig.Recv().Type(), "net/http", "Client") {
			return 0, false
		}
		switch fn.Name() {
		case "Get", "Post", "PostForm", "Head":
			return 0, true
		}
		return 0, false
	}
	switch fn.Name() {
	case "NewRequest":
		return 1, true
	case "NewRequestWithContext":
		return 2, true
	case "Get", "Post", "PostForm", "Head":
		return 0, true
	}
	return 0, false
}

func isNamed(t types.Type, pkgPath, name string) bool {
	if ptr, ok := t.(*types.Pointer); ok {
		t = ptr.Elem()
	}
	named, ok := t.(*types.Named)
	if !ok || named.Obj() == nil {
		return false
	}
	return named.Obj().Name() == name && pkgPathOf(named.Obj()) == pkgPath
}

// requesters computes the module functions that build HTTP requests from a
// string parameter, directly or by forwarding the parameter to another
// requester. Callers of these functions supply the interesting literals.
func (p *program) requesters() map[*types.Func]bool {
	set := make(map[*types.Func]bool)
	for changed := true; changed; {
		changed = false
		for fn, fd := range p.funcs {
			if set[fn] {
				continue
			}
			if forwardsStringParam(fd, set) {
				set[fn] = true
				changed = true
			}
		}
	}
	return set
}

// forwardsStringParam reports whether fd passes one of its own string
// parameters (possibly inside a larger expression) to a request builder or
// to an already-known requester.
func forwardsStringParam(fd *funcDecl, set map[*types.Func]bool) bool {
	info := fd.pkg.TypesInfo
	params := stringParams(info, fd.decl)
	if len(params) == 0 {
		return false
	}
	mentions := func(expr ast.Expr) bool { return mentionsAny(info, expr, params) }
	found := false
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := calleeFunc(info, call)
		if callee == nil {
			return true
		}
		if idx, ok := urlArgIndex(callee); ok {
			found = idx < len(call.Args) && mentions(call.Args[idx])
			return !found
		}
		if set[callee] && slices.ContainsFunc(call.Args, mentions) {
			found = true
		}
		return !found
	})
	return found
}

// eachStringConst visits the outermost constant string sub-expressions of
// expr, so a folded concatenation is seen whole and a format string inside
// fmt.Sprintf is still reached.
func eachStringConst(info *types.Info, expr ast.Expr, visit func(ast.Expr)) {
	ast.Inspect(expr, func(n ast.Node) bool {
		e, ok := n.(ast.Expr)
		if !ok {
			return true
		}
		if _, ok := constString(info, e); ok {
			visit(e)
			return false
		}
		return true
	})
}
