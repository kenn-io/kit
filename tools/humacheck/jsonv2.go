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
}

// constructionSites finds every call that hands a huma.Config to Huma (or to
// a module function that forwards it to Huma) in production code.
func (p *program) constructionSites() []constructionSite {
	sinks := p.configSinks()
	var sites []constructionSite
	p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			args := configArgs(pkg.TypesInfo, call, sinks)
			if len(args) > 0 {
				sites = append(sites, constructionSite{pkg: pkg, fn: fn, call: call, args: args})
			}
			return true
		})
	})
	return sites
}

// configArgs returns the arguments of call that are consumed as a
// huma.Config by Huma itself or by a known sink wrapper.
func configArgs(info *types.Info, call *ast.CallExpr, sinks map[*types.Func][]int) []ast.Expr {
	fn := calleeFunc(info, call)
	if fn == nil {
		return nil
	}
	var indexes []int
	if isHumaPkgPath(pkgPathOf(fn)) {
		sig, ok := fn.Type().(*types.Signature)
		if !ok {
			return nil
		}
		for i, param := range slices.Collect(sig.Params().Variables()) {
			if isHumaNamed(param.Type(), "Config") {
				indexes = append(indexes, i)
			}
		}
	} else {
		indexes = sinks[fn]
	}
	var args []ast.Expr
	for _, i := range indexes {
		if i < len(call.Args) {
			args = append(args, call.Args[i])
		}
	}
	return args
}

// configSinks maps module functions to the indexes of their huma.Config
// parameters that flow into a construction call. Computed to a fixpoint so
// wrappers of wrappers are recognized.
func (p *program) configSinks() map[*types.Func][]int {
	sinks := make(map[*types.Func][]int)
	for changed := true; changed; {
		changed = false
		for fn, fd := range p.funcs {
			params := paramObjects(fd.pkg.TypesInfo, fd.decl)
			for i, param := range params {
				if param == nil || !isHumaNamed(param.Type(), "Config") || slices.Contains(sinks[fn], i) {
					continue
				}
				if paramFlowsToConfig(fd, param, sinks) {
					sinks[fn] = append(sinks[fn], i)
					changed = true
				}
			}
		}
	}
	return sinks
}

func paramFlowsToConfig(fd *funcDecl, param *types.Var, sinks map[*types.Func][]int) bool {
	found := false
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		if found {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		for _, arg := range configArgs(fd.pkg.TypesInfo, call, sinks) {
			if ident, ok := ast.Unparen(arg).(*ast.Ident); ok && fd.pkg.TypesInfo.Uses[ident] == param {
				found = true
			}
		}
		return !found
	})
	return found
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

func checkJSONV2(p *program, sites []constructionSite) []Diagnostic {
	c := &jsonV2Checker{
		p:            p,
		formatFuncs:  make(map[*types.Func]verdict),
		configFuncs:  make(map[*types.Func]verdict),
		passthroughs: make(map[*types.Func]int),
		overriding:   make(map[*types.Package]bool),
		imports:      make(map[*types.Package]bool),
	}
	c.findOverrides()
	c.findDefaultFormatAssignments()
	for _, site := range sites {
		for _, arg := range site.args {
			v := c.configExpr(site.pkg, site.fn, arg, 0)
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

// configExpr judges an expression of type huma.Config inside fn.
func (c *jsonV2Checker) configExpr(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, depth int) verdict {
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
				return c.configExpr(pkg, fn, e.Args[idx], depth+1)
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
			return passthrough
		}
		return c.configVar(pkg, fn, obj, depth)
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
				return c.formatsMap(pkg, fn, kv.Value, depth+1)
			}
		}
		return no
	case *ast.UnaryExpr:
		if e.Op == token.AND {
			return c.configExpr(pkg, fn, e.X, depth+1)
		}
	case *ast.StarExpr:
		return c.configExpr(pkg, fn, e.X, depth+1)
	}
	return unknown
}

func isParam(info *types.Info, fn *ast.FuncDecl, obj *types.Var) bool {
	for _, param := range paramObjects(info, fn) {
		if param != nil && param == obj {
			return true
		}
	}
	return false
}

// configVar judges a local config variable from its assignments and from
// any Formats mutation applied to it within fn.
func (c *jsonV2Checker) configVar(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, depth int) verdict {
	info := pkg.TypesInfo
	result := unknown
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		if result == yes {
			return
		}
		ident, ok := ast.Unparen(lhs).(*ast.Ident)
		if !ok || (info.Defs[ident] != obj && info.Uses[ident] != obj) {
			return
		}
		switch c.configExpr(pkg, fn, rhs, depth+1) {
		case yes:
			result = yes
		case no:
			if result == unknown {
				result = no
			}
		}
	})
	if result == yes {
		return yes
	}
	if c.formatsMutated(pkg, fn, obj, depth) {
		return yes
	}
	return result
}

// formatsMutated reports whether fn assigns a JSON v2 format into
// obj.Formats, either wholesale or for the application/json key.
func (c *jsonV2Checker) formatsMutated(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, depth int) bool {
	info := pkg.TypesInfo
	found := false
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		if found {
			return
		}
		switch l := ast.Unparen(lhs).(type) {
		case *ast.SelectorExpr:
			if l.Sel.Name != "Formats" || !refersTo(info, l.X, obj) {
				return
			}
			found = c.formatsMap(pkg, fn, rhs, depth+1) == yes
		case *ast.IndexExpr:
			sel, ok := ast.Unparen(l.X).(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Formats" || !refersTo(info, sel.X, obj) {
				return
			}
			if key, ok := constString(info, l.Index); ok && key == "application/json" {
				found = c.formatExpr(pkg, fn, rhs, depth+1) == yes
			}
		}
	})
	return found
}

func refersTo(info *types.Info, expr ast.Expr, obj *types.Var) bool {
	ident := rootIdent(expr)
	if ident == nil {
		return false
	}
	return info.Uses[ident] == obj || info.Defs[ident] == obj
}

// configFunc judges a function that returns a huma.Config.
func (c *jsonV2Checker) configFunc(fn *types.Func, fd *funcDecl) verdict {
	if v, ok := c.configFuncs[fn]; ok {
		return v
	}
	c.configFuncs[fn] = unknown // cycle guard
	info := fd.pkg.TypesInfo
	result := unknown
	consider := func(v verdict) {
		switch v {
		case yes:
			result = yes
		case no:
			if result == unknown {
				result = no
			}
		}
	}
	params := paramObjects(info, fd.decl)
	ast.Inspect(fd.decl.Body, func(n ast.Node) bool {
		if result == yes {
			return false
		}
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
					consider(c.configVar(fd.pkg, fd.decl, res, 0))
				}
			}
			return true
		}
		for _, expr := range ret.Results {
			if !isHumaNamed(info.TypeOf(expr), "Config") {
				continue
			}
			v := c.configExpr(fd.pkg, fd.decl, expr, 1)
			if v != passthrough {
				consider(v)
				continue
			}
			// A returned parameter: it may have been reconfigured in place.
			ident, _ := ast.Unparen(expr).(*ast.Ident)
			param, _ := info.Uses[ident].(*types.Var)
			if v := c.configVar(fd.pkg, fd.decl, param, 1); v == yes {
				result = yes
				continue
			}
			for i, candidate := range params {
				if candidate == param {
					c.passthroughs[fn] = i
				}
			}
			if result == unknown {
				result = passthrough
			}
		}
		return true
	})
	c.configFuncs[fn] = result
	return result
}

// formatsMap judges an expression used as Config.Formats.
func (c *jsonV2Checker) formatsMap(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, depth int) verdict {
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
				return c.formatExpr(pkg, fn, kv.Value, depth+1)
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
		return c.mapVar(pkg, fn, obj, depth)
	}
	return unknown
}

// mapVar judges a local map[string]huma.Format variable by its literal
// initializer or by an application/json index assignment.
func (c *jsonV2Checker) mapVar(pkg *packages.Package, fn *ast.FuncDecl, obj *types.Var, depth int) verdict {
	info := pkg.TypesInfo
	result := unknown
	assignments(fn.Body, func(lhs, rhs ast.Expr) {
		if result == yes {
			return
		}
		switch l := ast.Unparen(lhs).(type) {
		case *ast.Ident:
			if info.Defs[l] != obj && info.Uses[l] != obj {
				return
			}
			if lit, ok := ast.Unparen(rhs).(*ast.CompositeLit); ok {
				if v := c.formatsMap(pkg, fn, lit, depth+1); v != unknown {
					result = v
				}
			}
		case *ast.IndexExpr:
			if !refersTo(info, l.X, obj) {
				return
			}
			if key, ok := constString(info, l.Index); ok && key == "application/json" {
				if v := c.formatExpr(pkg, fn, rhs, depth+1); v != unknown {
					result = v
				}
			}
		}
	})
	return result
}

// formatExpr judges an expression of type huma.Format.
func (c *jsonV2Checker) formatExpr(pkg *packages.Package, fn *ast.FuncDecl, expr ast.Expr, depth int) verdict {
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
		if marshal == yes && unmarshal == yes {
			return yes
		}
		if marshal == no || unmarshal == no || marshal == unknown || unmarshal == unknown {
			return no
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
		result := unknown
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			if result != unknown {
				return
			}
			if ident, ok := ast.Unparen(lhs).(*ast.Ident); ok && (info.Defs[ident] == obj || info.Uses[ident] == obj) {
				result = c.formatExpr(pkg, fn, rhs, depth+1)
			}
		})
		return result
	}
	return unknown
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

// findOverrides records packages that replace Huma's process-wide default
// JSON format with a JSON v2 format. Any package importing one of these
// (transitively) inherits the override through huma.DefaultConfig.
func (c *jsonV2Checker) findOverrides() {
	c.p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		info := pkg.TypesInfo
		jsonFormatOverridden := false
		assignments(fn.Body, func(lhs, rhs ast.Expr) {
			if path, obj := selectorOf(info, lhs); path == humaPath && obj.Name() == "DefaultJSONFormat" {
				if c.formatExpr(pkg, fn, rhs, 0) == yes {
					jsonFormatOverridden = true
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
			if c.formatExpr(pkg, fn, rhs, 0) == yes {
				c.overriding[pkg.Types] = true
				return
			}
			if p, o := selectorOf(info, rhs); p == humaPath && o.Name() == "DefaultJSONFormat" && jsonFormatOverridden {
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
