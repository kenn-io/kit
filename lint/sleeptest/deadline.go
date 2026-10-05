package sleeptest

import (
	"go/ast"
	"go/constant"
	"go/types"
	"time"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
)

// DeadlineAnalyzer reports sub-second wall-clock budgets in _test.go files
// outside synctest bubbles.
var DeadlineAnalyzer = &analysis.Analyzer{
	Name:     "deadlinetest",
	Doc:      "reports sub-second context deadlines, timers, and testify polling budgets in test files outside a testing/synctest bubble",
	Requires: []*analysis.Analyzer{inspect.Analyzer},
	Run:      runDeadline,
}

const deadlineMessage = "%s with a %s budget in a test outside a synctest bubble races the wall clock; wait on a real event, or run the test under testing/synctest"

func runDeadline(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inBubble := bubbleChecker(pass, inspect)
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isTestFile(pass, call) {
			return
		}
		arg := budgetArg(pass, call)
		if arg == nil {
			return
		}
		budget, ok := constantDuration(pass, arg)
		if !ok || budget <= 0 || budget >= time.Second || inBubble(call) {
			return
		}
		pass.Reportf(call.Pos(), deadlineMessage, calleeName(pass, call), budget)
	})
	return nil, nil
}

// budgetArg returns the duration argument of a call that waits on the wall
// clock, or nil when the call is not one of those.
func budgetArg(pass *analysis.Pass, call *ast.CallExpr) ast.Expr {
	switch {
	case isPackageFunc(pass, call, "context", "WithTimeout", "WithTimeoutCause"):
		return argAt(call, 1)
	case isPackageFunc(pass, call, "context", "WithDeadline", "WithDeadlineCause"):
		return nowPlus(pass, argAt(call, 1))
	case isPackageFunc(pass, call, "time", "After", "NewTimer", "AfterFunc"):
		return argAt(call, 0)
	case isPollingAssertion(pass, call):
		return argAt(call, waitForIndex(pass, call))
	}
	return nil
}

// nowPlus returns d when expr is written directly as time.Now().Add(d).
func nowPlus(pass *analysis.Pass, expr ast.Expr) ast.Expr {
	add, ok := expr.(*ast.CallExpr)
	if !ok {
		return nil
	}
	sel, ok := add.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Add" {
		return nil
	}
	now, ok := sel.X.(*ast.CallExpr)
	if !ok || !isPackageFunc(pass, now, "time", "Now") {
		return nil
	}
	return argAt(add, 0)
}

// waitForIndex finds the waitFor parameter by name so package functions and
// *Assertions methods, which differ by the leading t, resolve alike.
func waitForIndex(pass *analysis.Pass, call *ast.CallExpr) int {
	sel := call.Fun.(*ast.SelectorExpr)
	fn, ok := pass.TypesInfo.Uses[sel.Sel].(*types.Func)
	if !ok {
		return -1
	}
	params := fn.Signature().Params()
	for i := range params.Len() {
		if params.At(i).Name() == "waitFor" {
			return i
		}
	}
	return -1
}

func argAt(call *ast.CallExpr, i int) ast.Expr {
	if i < 0 || i >= len(call.Args) {
		return nil
	}
	return call.Args[i]
}

func constantDuration(pass *analysis.Pass, expr ast.Expr) (time.Duration, bool) {
	if expr == nil {
		return 0, false
	}
	value := pass.TypesInfo.Types[expr].Value
	if value == nil {
		return 0, false
	}
	n, exact := constant.Int64Val(constant.ToInt(value))
	return time.Duration(n), exact
}
