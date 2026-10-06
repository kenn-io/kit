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

const deadlineMessage = "%s with budget %s in a test outside a synctest bubble races the wall clock; wait on a real event, or run the test under testing/synctest"

func runDeadline(pass *analysis.Pass) (any, error) {
	inspect := pass.ResultOf[inspect.Analyzer].(*inspector.Inspector)
	inBubble := bubbleChecker(pass, inspect)
	inspect.Preorder([]ast.Node{(*ast.CallExpr)(nil)}, func(n ast.Node) {
		call := n.(*ast.CallExpr)
		if !isTestFile(pass, call) {
			return
		}
		arg, kind := budgetArg(pass, call)
		if arg == nil {
			return
		}
		budget, ok := constantDuration(pass, arg)
		if !ok || !kind.reports(budget) || inBubble(call) {
			return
		}
		pass.Reportf(call.Pos(), deadlineMessage, calleeName(pass, call), budget)
	})
	return nil, nil
}

// budgetKind says which constant budgets of a wall-clock call are reported.
type budgetKind int

const (
	// timerBudget reports positive sub-second budgets; a zero or negative
	// timer fires at once, so its outcome does not depend on timing.
	timerBudget budgetKind = iota
	// pollingBudget reports every sub-second budget; a zero one still passes
	// or fails by timing.
	pollingBudget
	// absenceBudget reports zero and negative budgets, which pass vacuously.
	// A short positive one checks fewer times under load but cannot fail.
	absenceBudget
)

func (k budgetKind) reports(budget time.Duration) bool {
	switch k {
	case timerBudget:
		return budget > 0 && budget < time.Second
	case pollingBudget:
		return budget < time.Second
	case absenceBudget:
		return budget <= 0
	}
	return false
}

// budgetArg returns the duration argument of a call that waits on the wall
// clock, or nil when the call is not one of those, and which of its budgets
// are reported.
func budgetArg(pass *analysis.Pass, call *ast.CallExpr) (ast.Expr, budgetKind) {
	switch {
	case isPackageFunc(pass, call, "context", "WithTimeout", "WithTimeoutCause"):
		return argAt(call, 1), timerBudget
	case isPackageFunc(pass, call, "context", "WithDeadline", "WithDeadlineCause"):
		return nowPlus(pass, argAt(call, 1)), timerBudget
	case isPackageFunc(pass, call, "time", "After", "NewTimer", "AfterFunc"):
		return argAt(call, 0), timerBudget
	case isTestifyFunc(pass, call, "Never", "Neverf"):
		return argAt(call, waitForIndex(pass, call)), absenceBudget
	case isTestifyFunc(pass, call, pollingAssertions...):
		return argAt(call, waitForIndex(pass, call)), pollingBudget
	}
	return nil, timerBudget
}

// nowPlus returns d when expr is written directly as time.Now().Add(d).
func nowPlus(pass *analysis.Pass, expr ast.Expr) ast.Expr {
	add, ok := ast.Unparen(expr).(*ast.CallExpr)
	if !ok {
		return nil
	}
	if !isPackageFunc(pass, add, "time", "Add") {
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
