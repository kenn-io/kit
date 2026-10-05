package sleeptest

import (
	"go/ast"
	"go/constant"
	"go/types"
	"time"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/inspect"
	"golang.org/x/tools/go/ast/inspector"
	"golang.org/x/tools/go/types/typeutil"
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
		arg, polling := budgetArg(pass, call)
		if arg == nil {
			return
		}
		budget, ok := constantDuration(pass, arg)
		// A zero or negative timer is deterministic; a zero polling budget still passes or fails by timing.
		if !ok || (budget <= 0 && !polling) || budget >= time.Second || inBubble(call) {
			return
		}
		pass.Reportf(call.Pos(), deadlineMessage, calleeName(pass, call), budget)
	})
	return nil, nil
}

// budgetArg returns the duration argument of a call that waits on the wall
// clock, or nil when the call is not one of those, and whether the call is a
// testify polling assertion.
func budgetArg(pass *analysis.Pass, call *ast.CallExpr) (ast.Expr, bool) {
	switch {
	case isPackageFunc(pass, call, "context", "WithTimeout", "WithTimeoutCause"):
		return argAt(call, 1), false
	case isPackageFunc(pass, call, "context", "WithDeadline", "WithDeadlineCause"):
		return nowPlus(pass, argAt(call, 1)), false
	case isPackageFunc(pass, call, "time", "After", "NewTimer", "AfterFunc"):
		return argAt(call, 0), false
	case isPollingAssertion(pass, call):
		return argAt(call, waitForIndex(pass, call)), true
	}
	return nil, false
}

// nowPlus returns d when expr is written directly as time.Now().Add(d).
func nowPlus(pass *analysis.Pass, expr ast.Expr) ast.Expr {
	add, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return nil
	}
	fn := typeutil.StaticCallee(pass.TypesInfo, add)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "time" || fn.Name() != "Add" {
		return nil
	}
	sel, ok := ast.Unparen(add.Fun).(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	now, ok := ast.Unparen(sel.X).(*ast.CallExpr)
	if !ok || !isPackageFunc(pass, now, "time", "Now") {
		return nil
	}
	return argAt(add, 0)
}

// waitForIndex finds the waitFor parameter by name in the call's own
// signature, so package functions, *Assertions methods, and method
// expressions, which differ by a leading t or receiver, resolve alike.
func waitForIndex(pass *analysis.Pass, call *ast.CallExpr) int {
	sig, ok := pass.TypesInfo.TypeOf(call.Fun).(*types.Signature)
	if !ok {
		return -1
	}
	params := sig.Params()
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
	value := pass.TypesInfo.Types[expr].Value
	if value == nil {
		return 0, false
	}
	n, exact := constant.Int64Val(constant.ToInt(value))
	return time.Duration(n), exact
}
