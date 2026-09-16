// Package sleeptest reports time.Sleep calls in test files that run outside a
// testing/synctest bubble. Real sleeps make tests slow and timing-dependent;
// inside synctest.Test the sleep advances a fake clock instead.
package sleeptest

import (
	"go/ast"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// Analyzer reports time.Sleep calls in _test.go files outside synctest bubbles.
var Analyzer = &analysis.Analyzer{
	Name:     "sleeptest",
	Doc:      "reports time.Sleep in test files outside a testing/synctest bubble",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      run,
}

const diagnosticMessage = "time.Sleep in a test outside a synctest bubble; run the test body under synctest.Test or wait on a real signal instead of wall-clock time"

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// bubbles holds the source ranges of function bodies passed to
	// synctest.Test: literals written inline, and the declarations or
	// literals behind identifiers passed by name. Sleeps inside those ranges
	// are fine.
	bodies := functionBodies(pass)
	var bubbles []ast.Node
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isPackageFunc(pass, call, "testing/synctest", "Test") {
			return
		}
		for _, arg := range call.Args {
			switch arg := arg.(type) {
			case *ast.FuncLit:
				bubbles = append(bubbles, arg)
			case *ast.Ident:
				if body, ok := bodies[pass.TypesInfo.Uses[arg]]; ok {
					bubbles = append(bubbles, body)
				}
			}
		}
	})

	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isTestFile(pass, call) || !isPackageFunc(pass, call, "time", "Sleep") {
			return
		}
		for _, bubble := range bubbles {
			if call.Pos() >= bubble.Pos() && call.End() <= bubble.End() {
				return
			}
		}
		pass.Reportf(call.Pos(), "%s", diagnosticMessage)
	})

	return nil, nil
}

// functionBodies maps each function declared in the package, and each
// variable initialized with a function literal, to the node holding its body.
func functionBodies(pass *analysis.Pass) map[types.Object]ast.Node {
	bodies := map[types.Object]ast.Node{}
	bind := func(name *ast.Ident, value ast.Expr) {
		if lit, ok := value.(*ast.FuncLit); ok {
			if obj := pass.TypesInfo.Defs[name]; obj != nil {
				bodies[obj] = lit
			}
		}
	}
	for _, file := range pass.Files {
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				if obj := pass.TypesInfo.Defs[n.Name]; obj != nil && n.Body != nil {
					bodies[obj] = n.Body
				}
			case *ast.AssignStmt:
				if len(n.Lhs) == len(n.Rhs) {
					for i, lhs := range n.Lhs {
						if name, ok := lhs.(*ast.Ident); ok {
							bind(name, n.Rhs[i])
						}
					}
				}
			case *ast.ValueSpec:
				if len(n.Names) == len(n.Values) {
					for i, name := range n.Names {
						bind(name, n.Values[i])
					}
				}
			}
			return true
		})
	}
	return bodies
}

func isTestFile(pass *analysis.Pass, n ast.Node) bool {
	return strings.HasSuffix(pass.Fset.Position(n.Pos()).Filename, "_test.go")
}

func isPackageFunc(pass *analysis.Pass, call *ast.CallExpr, pkgPath string, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok || fn.Pkg() == nil || fn.Pkg().Path() != pkgPath {
		return false
	}
	for _, name := range names {
		if fn.Name() == name {
			return true
		}
	}
	return false
}
