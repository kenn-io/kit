// Package sqlownership extends the SQL analyzers with ownership transfers and
// cross-package summaries of scanners that check iteration errors.
package sqlownership

import (
	"go/token"
	"go/types"
	"reflect"
	"slices"

	"github.com/golangci/rowserrcheck/passes/rowserr"
	sqlclose "github.com/ryanrolds/sqlclosecheck/pkg/analyzer"
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/ssa"
)

type scannerFact struct{ Parameters []int }

func (*scannerFact) AFact() {}

type (
	handled struct{ returned, scanned bool }
	result  map[token.Pos]handled
)

var ownership = &analysis.Analyzer{
	Name: "sqlownership", Doc: "Tracks SQL resource transfers and scanner error checks",
	Requires:   []*analysis.Analyzer{buildssa.Analyzer},
	FactTypes:  []analysis.Fact{new(scannerFact)},
	ResultType: reflect.TypeFor[result](), Run: analyze,
}

// CloseAnalyzer retains sqlclosecheck's diagnostics except for proven transfers.
var CloseAnalyzer = extend(sqlclose.NewAnalyzer(), false)

// ErrAnalyzer retains rowserrcheck's diagnostics except for transfers and checked scanners.
var ErrAnalyzer = extend(rowserr.NewAnalyzer(), true)

func extend(base *analysis.Analyzer, iteration bool) *analysis.Analyzer {
	a := *base
	a.Requires = append(append([]*analysis.Analyzer{}, base.Requires...), ownership)
	a.Run = func(pass *analysis.Pass) (any, error) {
		transfers := pass.ResultOf[ownership].(result)
		copyPass := *pass
		copyPass.Report = func(d analysis.Diagnostic) {
			h := transfers[d.Pos]
			if h.returned || iteration && h.scanned {
				return
			}
			pass.Report(d)
		}
		return base.Run(&copyPass)
	}
	return &a
}

type summaries map[*ssa.Function]map[int]bool

func analyze(pass *analysis.Pass) (any, error) {
	program := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	scans := summaries{}
	// Reach a fixed point for scanners forwarding to other local scanners.
	for changed := true; changed; {
		changed = false
		for _, fn := range program.SrcFuncs {
			for i, param := range fn.Params {
				if scans[fn][i] {
					continue
				}
				if checked(pass, scans, param, map[ssa.Value]bool{}) {
					if scans[fn] == nil {
						scans[fn] = map[int]bool{}
					}
					scans[fn][i] = true
					changed = true
				}
			}
		}
	}
	for _, fn := range program.SrcFuncs {
		obj, ok := fn.Object().(*types.Func)
		if !ok {
			continue
		}
		fact := new(scannerFact)
		for i := range fn.Params {
			if scans[fn][i] {
				fact.Parameters = append(fact.Parameters, i)
			}
		}
		if len(fact.Parameters) > 0 {
			pass.ExportObjectFact(obj, fact)
		}
	}
	out := result{}
	for _, fn := range program.SrcFuncs {
		for _, block := range fn.Blocks {
			for _, instr := range block.Instrs {
				call, ok := instr.(*ssa.Call)
				if !ok {
					continue
				}
				values := []ssa.Value{call}
				if _, tuple := call.Type().(*types.Tuple); tuple {
					values = nil
					for _, ref := range refs(call) {
						if v, ok := ref.(*ssa.Extract); ok {
							values = append(values, v)
						}
					}
				}
				for _, value := range values {
					if !resource(value.Type()) {
						continue
					}
					h, exists := out[call.Pos()]
					if !exists {
						h = handled{returned: true, scanned: true}
					}
					transferred := returned(value, map[ssa.Value]bool{})
					h.returned = h.returned && transferred
					h.scanned = h.scanned && (transferred || checked(pass, scans, value, map[ssa.Value]bool{}))
					out[call.Pos()] = h
				}
			}
		}
	}
	return out, nil
}

func refs(v ssa.Value) []ssa.Instruction {
	if v.Referrers() == nil {
		return nil
	}
	return *v.Referrers()
}

func resource(t types.Type) bool {
	if p, ok := t.(*types.Pointer); ok {
		t = p.Elem()
	}
	n, ok := t.(*types.Named)
	if !ok || n.Obj().Pkg() == nil {
		return false
	}
	switch n.Obj().Pkg().Path() {
	case "database/sql", "github.com/jmoiron/sqlx", "github.com/jackc/pgx/v5", "github.com/jackc/pgx/v5/pgxpool":
		switch n.Obj().Name() {
		case "Rows", "Stmt", "NamedStmt":
			return true
		}
	}
	return false
}

// returned follows the specific resource, including named-result stack slots
// emitted by SSA for functions with defers. Storing in an unrelated object or
// returning some other resource does not transfer this resource.
func returned(v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	for _, ref := range refs(v) {
		switch r := ref.(type) {
		case *ssa.Return:
			if slices.Contains(r.Results, v) {
				return true
			}
		case *ssa.MakeInterface:
			if returned(r, seen) {
				return true
			}
		case *ssa.ChangeType:
			if returned(r, seen) {
				return true
			}
		case *ssa.Phi:
			if returned(r, seen) {
				return true
			}
		case *ssa.Store:
			if r.Val != v {
				continue
			}
			if field, ok := r.Addr.(*ssa.FieldAddr); ok {
				if returned(field.X, seen) {
					return true
				}
			} else if slot, local := r.Addr.(*ssa.Alloc); local {
				for _, load := range liveLoads(r, slot) {
					if returned(load, seen) {
						return true
					}
				}
			}
		case *ssa.UnOp:
			if r.Op == token.MUL && returned(r, seen) {
				return true
			}
		}
	}
	return false
}

// liveLoads returns the loads of slot that can observe the stored value. A
// later store to the same slot ends the path, so an overwritten resource is
// not treated as the one that reaches the return.
func liveLoads(store *ssa.Store, slot *ssa.Alloc) []*ssa.UnOp {
	var loads []*ssa.UnOp
	// scan reports whether the stored value survives to the end of instrs.
	scan := func(instrs []ssa.Instruction) bool {
		for _, instr := range instrs {
			switch i := instr.(type) {
			case *ssa.Store:
				if i.Addr == slot {
					return false
				}
			case *ssa.UnOp:
				if i.Op == token.MUL && i.X == slot {
					loads = append(loads, i)
				}
			}
		}
		return true
	}
	block := store.Block()
	var pending []*ssa.BasicBlock
	if scan(block.Instrs[slices.Index(block.Instrs, ssa.Instruction(store))+1:]) {
		pending = slices.Clone(block.Succs)
	}
	visited := map[*ssa.BasicBlock]bool{}
	for len(pending) > 0 {
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[next] {
			continue
		}
		visited[next] = true
		if scan(next.Instrs) {
			pending = append(pending, next.Succs...)
		}
	}
	return loads
}

func checked(pass *analysis.Pass, scans summaries, v ssa.Value, seen map[ssa.Value]bool) bool {
	if seen[v] {
		return false
	}
	seen[v] = true
	for _, ref := range refs(v) {
		switch r := ref.(type) {
		case *ssa.MakeInterface:
			if checked(pass, scans, r, seen) {
				return true
			}
		case *ssa.ChangeType:
			if checked(pass, scans, r, seen) {
				return true
			}
		case *ssa.Phi:
			if checked(pass, scans, r, seen) {
				return true
			}
		case *ssa.Call:
			common := r.Common()
			if errReceiver(common) == v && len(refs(r)) > 0 {
				return true
			}
			callee := common.StaticCallee()
			if callee == nil {
				continue
			}
			for i, arg := range common.Args {
				if arg != v {
					continue
				}
				if scans[callee][i] {
					return true
				}
				if obj, ok := callee.Object().(*types.Func); ok {
					var fact scannerFact
					if pass.ImportObjectFact(obj, &fact) {
						if slices.Contains(fact.Parameters, i) {
							return true
						}
					}
				}
			}
		}
	}
	return false
}

func errReceiver(call *ssa.CallCommon) ssa.Value {
	if call.IsInvoke() {
		if call.Method.Name() == "Err" && call.Signature().Params().Len() == 0 && returnsError(call.Signature()) {
			return call.Value
		}
	} else if fn := call.StaticCallee(); fn != nil && fn.Name() == "Err" && fn.Signature.Recv() != nil && fn.Signature.Params().Len() == 0 && returnsError(fn.Signature) && len(call.Args) == 1 {
		return call.Args[0]
	}
	return nil
}

func returnsError(sig *types.Signature) bool {
	return sig.Results().Len() == 1 && types.Identical(sig.Results().At(0).Type(), types.Universe.Lookup("error").Type())
}
