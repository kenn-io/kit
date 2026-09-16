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

	// bubbles holds the source ranges of function literals passed to
	// synctest.Test. Sleeps inside those ranges are fine.
	var bubbles []*ast.FuncLit
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isPackageFunc(pass, call, "testing/synctest", "Test") {
			return
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				bubbles = append(bubbles, lit)
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
