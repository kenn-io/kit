package humacheck

import (
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/packages"
)

const (
	jsonV2Message = "Huma API is configured with encoding/json (v1) formats; set config.Formats[\"application/json\"] to a huma.Format whose Marshal and Unmarshal use encoding/json/v2"
	jsonV2Unknown = "cannot verify that this Huma API uses encoding/json/v2 formats; build the config in a function that sets config.Formats[\"application/json\"] to a huma.Format backed by encoding/json/v2"
	jsonV2Default = "huma.DefaultFormats and huma.DefaultJSONFormat encode with encoding/json (v1); use huma.Format values backed by encoding/json/v2"
)

// verdict is a three-valued answer for "does this use JSON v2".
type verdict int

const (
	unknown verdict = iota
	yes
	no
	passthrough // a config parameter; the caller is judged instead
)

const maxDepth = 6

// constructionSite is a call that turns a huma.Config into a live API.
type constructionSite struct {
	pkg  *packages.Package
	fn   *ast.FuncDecl
	call *ast.CallExpr
	args []ast.Expr // config arguments
	// settled marks config arguments the callee reconfigures itself, so the
	// argument value does not matter.
	settled []bool
}

// sinkParam describes one huma.Config parameter of a module function that
// flows into a construction call.
type sinkParam struct {
	index   int
	settled bool // the wrapper installs JSON v2 before construction
}

// constructionSites finds every call that hands a huma.Config to Huma (or to
// a module function that forwards it to Huma) in production code.
func (p *program) constructionSites(c *jsonV2Checker) []constructionSite {
	sinks := c.configSinks()
	var sites []constructionSite
	p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			args, settled := configArgs(pkg.TypesInfo, call, sinks)
			if len(args) > 0 {
				sites = append(sites, constructionSite{pkg: pkg, fn: fn, call: call, args: args, settled: settled})
			}
			return true
		})
	})
	return sites
}

// configArgs returns the arguments of call that are consumed as a
// huma.Config by Huma itself or by a known sink wrapper.
func configArgs(info *types.Info, call *ast.CallExpr, sinks map[*types.Func][]sinkParam) ([]ast.Expr, []bool) {
	fn := calleeFunc(info, call)
	if fn == nil {
		return nil, nil
	}
	var params []sinkParam
	if isHumaPkgPath(pkgPathOf(fn)) {
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return nil, nil
		}
		for i, param := range slices.Collect(sig.Params().Variables()) {
			if isHumaNamed(param.Type(), "Config") {
				params = append(params, sinkParam{index: i})
			}
		}
	} else {
		params = sinks[fn]
	}
	var args []ast.Expr
	var settled []bool
	for _, param := range params {
		if param.index < len(call.Args) {
			args = append(args, call.Args[param.index])
			settled = append(settled, param.settled)
		}
	}
	return args, settled
}

// configSinks maps module functions to their huma.Config parameters that
// flow into a construction call, computed to a fixpoint so wrappers of
// wrappers are recognized. A wrapper that installs JSON v2 on the parameter
// before constructing settles the verdict for every caller.
func (c *jsonV2Checker) configSinks() map[*types.Func][]sinkParam {
	sinks := make(map[*types.Func][]sinkParam)
	for changed := true; changed; {
		changed = false
		for fn, fd := range c.p.funcs {
			params := paramObjects(fd.pkg.TypesInfo, fd.decl)
			for i, param := range params {
				if param == nil || !isHumaNamed(param.Type(), "Config") {
					continue
				}
				if slices.ContainsFunc(sinks[fn], func(s sinkParam) bool { return s.index == i }) {
					continue
				}
				if pos, ok := paramFlowsToConfig(fd, param, sinks); ok {
					settled := c.configVar(fd.pkg, fd.decl, param, pos, 0) == yes
					sinks[fn] = append(sinks[fn], sinkParam{index: i, settled: settled})
					changed = true
				}
			}
		}
	}
	return sinks
}

// paramFlowsToConfig reports the first construction call in fd that receives
// param directly as a config argument.
func paramFlowsToConfig(fd *funcDecl, param *types.Var, sinks map[*types.Func][]sinkParam) (token.Pos, bool) {
	var found token.Pos
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		if found.IsValid() {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		args, _ := configArgs(fd.pkg.TypesInfo, call, sinks)
		for _, arg := range args {
			if ident, ok := ast.Unparen(arg).(*ast.Ident); ok && fd.pkg.TypesInfo.Uses[ident] == param {
				found = call.Pos()
			}
		}
		return !found.IsValid()
	})
	return found, found.IsValid()
}

// jsonV2Checker memoizes verdicts across the whole module.
type jsonV2Checker struct {
	p           *program
	formatFuncs map[*types.Func]verdict
	configFuncs map[*types.Func]verdict
	// passthroughs maps functions that return one of their huma.Config
	// parameters unchanged to that parameter's index; the argument at a call
	// site is judged instead.
	passthroughs map[*types.Func]int
	overriding   map[*types.Package]bool
	imports      map[*types.Package]bool
	diags        []Diagnostic
}

func newJSONV2Checker(p *program) *jsonV2Checker {
	return &jsonV2Checker{
		p:            p,
		formatFuncs:  make(map[*types.Func]verdict),
		configFuncs:  make(map[*types.Func]verdict),
		passthroughs: make(map[*types.Func]int),
		overriding:   make(map[*types.Package]bool),
		imports:      make(map[*types.Package]bool),
	}
}

func (c *jsonV2Checker) check(sites []constructionSite) []Diagnostic {
	c.findOverrides()
	c.findDefaultFormatAssignments()
	for _, site := range sites {
		for i, arg := range site.args {
			if site.settled[i] {
				continue
			}
			v := c.configExpr(site.pkg, site.fn, arg, site.call.Pos(), 0)
			if v == yes || v == passthrough || c.importsOverride(site.pkg.Types) {
				continue
			}
			message := jsonV2Unknown
			if v == no {
				message = jsonV2Message
			}
			c.report(arg.Pos(), message)
		}
	}
	return c.diags
}

func (c *jsonV2Checker) report(pos token.Pos, message string) {
	path, line, col := positionOf(c.p.fset, pos)
	c.diags = append(c.diags, Diagnostic{Path: path, Line: line, Column: col, Rule: RuleJSONV2, Message: message})
}

// configExpr judges an expression of type huma.Config inside fn as it stands
// at position before. Only assignments and mutations that textually precede
// that position count, so a reconfiguration after the API is built does not
// pass the check.
func (c *jsonV2Checker) configExpr(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, before token.Pos, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.CallExpr:
		callee := calleeFunc(info, e)
		if callee == nil {
			return unknown
		}
		if pkgPathOf(callee) == humaPath && callee.Name() == "DefaultConfig" {
			return no
		}
		if fd, ok := c.p.funcs[callee]; ok {
			v := c.configFunc(callee, fd)
			if idx, ok := c.passthroughs[callee]; v == passthrough && ok && idx < len(e.Args) {
				return c.configExpr(pkg, fn, e.Args[idx], before, depth+1)
			}
			return v
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		if isParam(info, fn, obj) {
			if v := c.configVar(pkg, fn, obj, before, depth); v == yes || v == no {
				return v
			}
			return passthrough
		}
		return c.configVar(pkg, fn, obj, before, depth)
	case *ast.CompositeLit:
		if !isHumaNamed(info.TypeOf(e), "Config") {
			return unknown
		}
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Formats" {
				return c.formatsMap(pkg, fn, kv.Value, before, depth+1)
			}
		}
		return no
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return c.configExpr(pkg, fn, e.X, before, depth+1)
		}
	case *ast.StarExpr:
		return c.configExpr(pkg, fn, e.X, before, depth+1)
	}
	return unknown
}

func isParam(info *types.Info, fn *ast.FuncDecl, obj *types.Var) bool {
	return slices.Contains(paramObjects(info, fn), obj)
}

// configVar judges a config variable as it stands at position before: the
// latest preceding assignment decides, and a JSON v2 Formats mutation
// between that assignment and before upgrades it. A parameter with no
// assignment is judged by its mutations alone.
func (c *jsonV2Checker) configVar(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, before token.Pos, depth int) verdict {
	info := pkg.TypesInfo
	var latest ast.Expr
	var latestPos token.Pos
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		ident, ok := ast.Unparen(lhs).(*ast.Ident)
		if !ok || (info.Defs[ident] != obj && info.Uses[ident] != obj) {
			return
		}
		if lhs.Pos() < before && lhs.Pos() > latestPos {
			latest, latestPos = rhs, lhs.Pos()
		}
	})
	result := unknown
	if latest != nil {
		result = c.configExpr(pkg, fn, latest, latestPos, depth+1)
	}
	if v, mutated := c.formatsMutation(pkg, fn, obj, latestPos, before, depth); mutated {
		return v
	}
	return result
}

// formatsMutation reports the effect of the last Formats mutation of obj
// between after and before: yes for a JSON v2 install, no for Huma's v1
// defaults, unknown when it cannot be followed. mutated is false when there
// is no such mutation.
func (c *jsonV2Checker) formatsMutation(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, after, before token.Pos, depth int) (result verdict, mutated bool) {
	info := pkg.TypesInfo
	var resultPos token.Pos
	consider := func(pos token.Pos, v verdict) {
		if pos <= after || pos >= before || pos < resultPos {
			return
		}
		result, resultPos, mutated = v, pos, true
	}
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		switch l := ast.Unparen(lhs).(type) {
		case *ast.SelectorExpr:
			if l.Sel.Name != "Formats" || !refersTo(info, l.X, obj) {
				return
			}
			consider(lhs.Pos(), c.formatsMap(pkg, fn, rhs, lhs.Pos(), depth+1))
		case *ast.IndexExpr:
			sel, ok := ast.Unparen(l.X).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Formats" || !refersTo(info, sel.X, obj) {
				return
			}
			if key, ok := constString(info, l.Index); ok && key == "application/json" {
				consider(lhs.Pos(), c.formatExpr(pkg, fn, rhs, lhs.Pos(), depth+1))
			}
		}
	})
	return result, mutated
}

func refersTo(info *types.Info, expr ast.Expr, obj *types.Var) bool {
	ident := rootIdent(expr)
	if ident == nil {
		return false
	}
	return info.Uses[ident] == obj || info.Defs[ident] == obj
}

// configFunc judges a function that returns a huma.Config. Every return
// must agree: one v1 return makes the function v1, one unverifiable return
// makes it unverifiable.
func (c *jsonV2Checker) configFunc(fn *types.Func, fd *funcDecl) verdict {
	if v, ok := c.configFuncs[fn]; ok {
		return v
	}
	c.configFuncs[fn] = unknown // cycle guard
	info := fd.pkg.TypesInfo
	params := paramObjects(info, fd.decl)
	var verdicts []verdict
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		ret, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(ret.Results) == 0 {
			for _, res := range resultObjects(info, fd.decl) {
				if res != nil && isHumaNamed(res.Type(), "Config") {
					verdicts = append(verdicts, c.configVar(fd.pkg, fd.decl, res, ret.Pos(), 0))
				}
			}
			return true
		}
		for _, expr := range ret.Results {
			if !isHumaNamed(info.TypeOf(expr), "Config") {
				continue
			}
			v := c.configExpr(fd.pkg, fd.decl, expr, ret.Pos(), 1)
			if v == passthrough {
				ident, _ := ast.Unparen(expr).(*ast.Ident)
				param, _ := info.Uses[ident].(*types.Var)
				if idx := slices.Index(params, param); idx >= 0 {
					c.passthroughs[fn] = idx
				}
			}
			verdicts = append(verdicts, v)
		}
		return true
	})
	result := combine(verdicts)
	c.configFuncs[fn] = result
	return result
}

// combine folds per-path verdicts: any no wins, then any unknown, then
// passthrough, and yes only when every path is yes.
func combine(verdicts []verdict) verdict {
	if len(verdicts) == 0 {
		return unknown
	}
	result := yes
	for _, v := range verdicts {
		switch v {
		case no:
			return no
		case unknown:
			result = unknown
		case passthrough:
			if result == yes {
				result = passthrough
			}
		}
	}
	return result
}

// formatsMap judges an expression used as Config.Formats.
func (c *jsonV2Checker) formatsMap(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, before token.Pos, depth int) verdict {
	if depth > maxDepth {
		return unknown
	}
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.CompositeLit:
		for _, elt := range e.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := constString(info, kv.Key); ok && key == "application/json" {
				return c.formatExpr(pkg, fn, kv.Value, before, depth+1)
			}
		}
		return no
	case *ast.SelectorExpr:
		if path, obj := selectorOf(info, e); path == humaPath && obj.Name() == "DefaultFormats" {
			return no
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		return c.mapVar(pkg, fn, obj, before, depth)
	}
	return unknown
}

// mapVar judges a local map[string]huma.Format variable by its latest
// literal initializer and any later application/json index assignment.
func (c *jsonV2Checker) mapVar(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, before token.Pos, depth int) verdict {
	info := pkg.TypesInfo
	result := unknown
	var resultPos token.Pos
	consider := func(pos token.Pos, v verdict) {
		if pos >= before || pos < resultPos || v == unknown {
			return
		}
		result, resultPos = v, pos
	}
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		switch l := ast.Unparen(lhs).(type) {
		case *ast.Ident:
			if info.Defs[l] != obj && info.Uses[l] != obj {
				return
			}
			if lit, ok := ast.Unparen(rhs).(*ast.CompositeLit); ok {
				consider(lhs.Pos(), c.formatsMap(pkg, fn, lit, lhs.Pos(), depth+1))
			}
		case *ast.IndexExpr:
			if !refersTo(info, l.X, obj) {
				return
			}
			if key, ok := constString(info, l.Index); ok && key == "application/json" {
				consider(lhs.Pos(), c.formatExpr(pkg, fn, rhs, lhs.Pos(), depth+1))
			}
		}
	})
	return result
}

// formatExpr judges an expression of type huma.Format: yes only when both
// codecs are proven v2, no only when either is proven v1, else unknown.
func (c *jsonV2Checker) formatExpr(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, before token.Pos, depth int) verdict {
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
				marshal = c.funcUsesV2(pkg, kv.Value, 0)
			case "Unmarshal":
				unmarshal = c.funcUsesV2(pkg, kv.Value, 0)
			}
		}
		switch {
		case marshal == no || unmarshal == no:
			return no
		case marshal == yes && unmarshal == yes:
			return yes
		}
		return unknown
	case *ast.SelectorExpr:
		if path, obj := selectorOf(info, e); path == humaPath && obj.Name() == "DefaultJSONFormat" {
			return no
		}
		return unknown
	case *ast.Ident:
		obj, ok := info.Uses[e].(*types.Var)
		if !ok {
			return unknown
		}
		if init := packageVarInit(pkg, obj); init != nil {
			return c.formatExpr(pkg, fn, init, init.Pos(), depth+1)
		}
		var latest ast.Expr
		var latestPos token.Pos
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			if ident, ok := ast.Unparen(lhs).(*ast.Ident); ok && (info.Defs[ident] == obj || info.Uses[ident] == obj) {
				if lhs.Pos() < before && lhs.Pos() > latestPos {
					latest, latestPos = rhs, lhs.Pos()
				}
			}
		})
		if latest == nil {
			return unknown
		}
		return c.formatExpr(pkg, fn, latest, latestPos, depth+1)
	}
	return unknown
}

// packageVarInit returns the initializer of a package-level variable, or
// nil when obj is not one or has none.
func packageVarInit(pkg *packages.Package, obj *types.Var) ast.Expr {
	if obj.Parent() != pkg.Types.Scope() {
		return nil
	}
	for _, file := range pkg.Syntax {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Names) != len(vs.Values) {
					continue
				}
				for i, name := range vs.Names {
					if pkg.TypesInfo.Defs[name] == obj {
						return vs.Values[i]
					}
				}
			}
		}
	}
	return nil
}

// funcUsesV2 judges a Marshal or Unmarshal value: a function literal or a
// reference to a function. It is yes only when the body reaches
// encoding/json/v2 and never calls an encoding/json (v1) codec function.
func (c *jsonV2Checker) funcUsesV2(pkg *packages.Package, expr ast.Expr, depth int) verdict {
	info := pkg.TypesInfo
	switch e := ast.Unparen(expr).(type) {
	case *ast.FuncLit:
		return c.bodyUsesV2(pkg, e.Body, depth)
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
		return c.funcDeclUsesV2(fn, depth)
	}
	return unknown
}

func (c *jsonV2Checker) funcDeclUsesV2(fn *types.Func, depth int) verdict {
	switch path := pkgPathOf(fn); {
	case path == jsonV2Path || path == jsonTextPath:
		return yes
	case path == jsonV1Path && isV1Codec(fn.Name()):
		return no
	}
	if v, ok := c.formatFuncs[fn]; ok {
		return v
	}
	fd, ok := c.p.funcs[fn]
	if !ok {
		return unknown
	}
	c.formatFuncs[fn] = unknown // cycle guard
	v := c.bodyUsesV2(fd.pkg, fd.decl.Body, depth+1)
	c.formatFuncs[fn] = v
	return v
}

// bodyUsesV2 scans calls in body. Local callees are followed so thin
// wrappers such as marshalAPIJSON count.
func (c *jsonV2Checker) bodyUsesV2(pkg *packages.Package, body *ast.BlockStmt, depth int) verdict {
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
			if _, local := c.p.funcs[fn]; local {
				switch c.funcDeclUsesV2(fn, depth+1) {
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

// findOverrides records packages whose init functions replace Huma's
// process-wide default JSON format with a JSON v2 format. Only init runs
// unconditionally before any API is built, so assignments elsewhere are
// not trusted. Any package importing an overriding package (transitively)
// inherits the override through huma.DefaultConfig.
func (c *jsonV2Checker) findOverrides() {
	c.p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		if fn.Recv != nil || fn.Name.Name != "init" {
			return
		}
		info := pkg.TypesInfo
		var jsonFormatOverridden token.Pos
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			if path, obj := selectorOf(info, lhs); path == humaPath && obj.Name() == "DefaultJSONFormat" {
				if c.formatExpr(pkg, fn, rhs, lhs.Pos(), 0) == yes {
					jsonFormatOverridden = lhs.Pos()
				}
			}
		})
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			idx, ok := ast.Unparen(lhs).(*ast.IndexExpr)
			if !ok {
				return
			}
			path, obj := selectorOf(info, idx.X)
			if path != humaPath || obj.Name() != "DefaultFormats" {
				return
			}
			if key, ok := constString(info, idx.Index); !ok || key != "application/json" {
				return
			}
			if c.formatExpr(pkg, fn, rhs, lhs.Pos(), 0) == yes {
				c.overriding[pkg.Types] = true
				return
			}
			p, o := selectorOf(info, rhs)
			if p == humaPath && o.Name() == "DefaultJSONFormat" && jsonFormatOverridden.IsValid() && jsonFormatOverridden < lhs.Pos() {
				c.overriding[pkg.Types] = true
			}
		})
	})
}

// findDefaultFormatAssignments reports explicit use of Huma's v1 defaults as
// a Formats value.
func (c *jsonV2Checker) findDefaultFormatAssignments() {
	c.p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		info := pkg.TypesInfo
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Formats" || !isHumaNamed(info.TypeOf(sel.X), "Config") {
				return
			}
			if path, obj := selectorOf(info, rhs); path == humaPath && obj.Name() == "DefaultFormats" {
				c.report(rhs.Pos(), jsonV2Default)
			}
		})
	})
}

func (c *jsonV2Checker) importsOverride(pkg *types.Package) bool {
	if pkg == nil {
		return false
	}
	if v, ok := c.imports[pkg]; ok {
		return v
	}
	c.imports[pkg] = false // cycle guard
	result := c.overriding[pkg]
	for _, imp := range pkg.Imports() {
		if result {
			break
		}
		result = c.importsOverride(imp)
	}
	c.imports[pkg] = result
	return result
}
