// Package sleeptest reports time.Sleep calls in test files that run outside a
// testing/synctest bubble. Real sleeps make tests slow and timing-dependent;
// inside synctest.Test the sleep advances a fake clock instead.
//
// A bubble is the body of a function passed to synctest.Test (or the older
// synctest.Run): a literal written inline, or a declared function or
// function-valued variable passed by name. Sleeps anywhere inside that body,
// including nested closures and goroutines, are fine. A helper that sleeps
// and is merely called from inside a bubble cannot be recognized statically
// and is reported; give such helpers a channel to wait on or a sleep
// function to call instead.
//
// Test files are *_test.go files. With the helper-packages flag (on by
// default) files in packages named testutil or ending in "test", such as
// pkgtest, are checked too. The eventually flag (off by default) also
// reports testify's Eventually, EventuallyWithT, and Never outside bubbles,
// for repositories that decide polling assertions should be replaced by
// signals.
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

const diagnosticMessage = "time.Sleep in a test outside a synctest bubble; wait on a channel or signal, or run the test under testing/synctest"

// HelperPackages extends the check from _test.go files to every file in a
// package named testutil or with a name ending in "test".
var HelperPackages = true

// Eventually also reports testify's Eventually, EventuallyWithT, and Never
// outside a bubble. They poll on a wall clock just like a sleep does.
var Eventually bool

func init() {
	Analyzer.Flags.BoolVar(&HelperPackages, "helper-packages", true, "also check packages named testutil or ending in \"test\"")
	Analyzer.Flags.BoolVar(&Eventually, "eventually", false, "also report testify Eventually, EventuallyWithT, and Never outside a bubble")
}

const eventuallyMessage = "%s in a test outside a synctest bubble polls the wall clock; wait on a channel or signal, or run the test under testing/synctest"

func run(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)

	// bubbles holds the source ranges of function bodies passed to
	// synctest.Test: literals written inline, and the declarations or
	// literals behind identifiers passed by name. Sleeps inside those ranges
	// are fine when every use of the named callback is through synctest.Test.
	bodies := functionBodies(pass)
	var bubbles []ast.Node
	uses := make(map[types.Object]int, len(bodies))
	for _, obj := range pass.TypesInfo.Uses {
		if _, ok := bodies[obj]; ok {
			uses[obj]++
		}
	}
	callbackUses := make(map[types.Object]int, len(bodies))
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isPackageFunc(pass, call, "testing/synctest", "Test", "Run") {
			return
		}
		for _, arg := range call.Args {
			switch arg := arg.(type) {
			case *ast.FuncLit:
				bubbles = append(bubbles, arg)
			case *ast.Ident:
				if obj := pass.TypesInfo.Uses[arg]; bodies[obj] != nil {
					callbackUses[obj]++
				}
			}
		}
	})
	for obj, count := range callbackUses {
		if count == uses[obj] {
			bubbles = append(bubbles, bodies[obj])
		}
	}

	inBubble := func(call *ast.CallExpr) bool {
		for _, bubble := range bubbles {
			if call.Pos() >= bubble.Pos() && call.End() <= bubble.End() {
				return true
			}
		}
		return false
	}
	helperPackage := HelperPackages && isHelperPackage(pass.Pkg.Name())
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !helperPackage && !isTestFile(pass, call) {
			return
		}
		switch {
		case isPackageFunc(pass, call, "time", "Sleep"):
			if !inBubble(call) {
				pass.Reportf(call.Pos(), "%s", diagnosticMessage)
			}
		case Eventually && isPollingAssertion(pass, call):
			if !inBubble(call) {
				pass.Reportf(call.Pos(), eventuallyMessage, calleeName(pass, call))
			}
		}
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

func isHelperPackage(name string) bool {
	name = strings.TrimSuffix(name, "_test")
	return name == "testutil" || strings.HasSuffix(name, "test")
}

func isPollingAssertion(pass *analysis.Pass, call *ast.CallExpr) bool {
	for _, path := range []string{"github.com/stretchr/testify/assert", "github.com/stretchr/testify/require"} {
		if isPackageFunc(pass, call, path, "Eventually", "EventuallyWithT", "Never") {
			return true
		}
	}
	return false
}

func calleeName(pass *analysis.Pass, call *ast.CallExpr) string {
	sel := call.Fun.(*ast.SelectorExpr)
	fn := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	return fn.Pkg().Name() + "." + fn.Name()
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
