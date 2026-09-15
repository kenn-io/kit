package humacheck

import (
	"go/ast"
	"go/types"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/tools/go/packages"
)

// routeSet is the inventory of HTTP routes the module registers with Huma.
type routeSet struct {
	prefixes []string // adapter prefixes seen, always includes ""
	paths    []string // registered operation paths
	concrete []string // prefix+path for every prefix
}

var paramPattern = regexp.MustCompile(`\{[^/}]*\}`)

// collectRoutes gathers operation paths from huma.Operation literals, the
// huma.Get/Post/... helpers, and module helpers that build or register an
// operation from a path parameter, plus every constant prefix handed to a
// Huma adapter or huma.NewGroup.
func collectRoutes(p *program) *routeSet {
	rs := &routeSet{prefixes: []string{""}}
	registrars := p.registrars()
	seenPath := map[string]bool{}
	seenPrefix := map[string]bool{"": true}
	addPath := func(path string) {
		if path == "" || !strings.HasPrefix(path, "/") || seenPath[path] {
			return
		}
		seenPath[path] = true
		rs.paths = append(rs.paths, path)
	}
	addPrefix := func(prefix string) {
		if seenPrefix[prefix] {
			return
		}
		seenPrefix[prefix] = true
		rs.prefixes = append(rs.prefixes, prefix)
	}

	p.eachFunc(func(pkg *packages.Package, _ *ast.File, fn *ast.FuncDecl) {
		info := pkg.TypesInfo
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if isHumaNamed(info.TypeOf(node), "Operation") {
					for _, elt := range node.Elts {
						kv, ok := elt.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Path" {
							if path, ok := constString(info, kv.Value); ok {
								addPath(path)
							}
						}
					}
				}
			case *ast.CallExpr:
				callee := calleeFunc(info, node)
				if callee == nil {
					return true
				}
				path := pkgPathOf(callee)
				switch {
				case path == humaPath && isHumaMethodHelper(callee.Name()):
					if len(node.Args) > 1 {
						if route, ok := constString(info, node.Args[1]); ok {
							addPath(route)
						}
					}
				case (isHumaPkgPath(path) && path != humaPath) || (path == humaPath && callee.Name() == "NewGroup"):
					// Adapter constructors and huma.NewGroup: any constant
					// string argument is a mount prefix.
					for _, arg := range node.Args {
						if prefix, ok := constString(info, arg); ok && strings.HasPrefix(prefix, "/") {
							addPrefix(prefix)
						}
					}
				case returnsOperation(callee) || registrars[callee]:
					for _, arg := range node.Args {
						if route, ok := constString(info, arg); ok && strings.HasPrefix(route, "/") {
							addPath(route)
						}
					}
				}
			}
			return true
		})
	})

	// Group prefixes nest under adapter prefixes; compose every pair so a
	// group mounted on a prefixed API is reachable at both spellings.
	base := slices.Clone(rs.prefixes)
	for _, outer := range base {
		for _, inner := range base {
			if outer != "" && inner != "" && outer != inner {
				addPrefix(outer + inner)
			}
		}
	}
	for _, prefix := range rs.prefixes {
		for _, path := range rs.paths {
			rs.concrete = append(rs.concrete, prefix+path)
		}
	}
	return rs
}

func isHumaMethodHelper(name string) bool {
	switch name {
	case "Get", "Post", "Put", "Patch", "Delete", "Head", "Options":
		return true
	}
	return false
}

// registrars computes module functions that register a route from one of
// their string parameters: the parameter reaches an Operation Path, a Huma
// method helper, an Operation builder, or another registrar. Constant
// arguments to these functions are routes.
func (p *program) registrars() map[*types.Func]bool {
	set := make(map[*types.Func]bool)
	for changed := true; changed; {
		changed = false
		for fn, fd := range p.funcs {
			if set[fn] {
				continue
			}
			if forwardsPathParam(fd, set) {
				set[fn] = true
				changed = true
			}
		}
	}
	return set
}

func forwardsPathParam(fd *funcDecl, set map[*types.Func]bool) bool {
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
		switch node := n.(type) {
		case *ast.CompositeLit:
			if !isHumaNamed(info.TypeOf(node), "Operation") {
				return true
			}
			for _, elt := range node.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Path" && mentions(kv.Value) {
						found = true
					}
				}
			}
		case *ast.CallExpr:
			callee := calleeFunc(info, node)
			if callee == nil {
				return true
			}
			switch {
			case pkgPathOf(callee) == humaPath && isHumaMethodHelper(callee.Name()):
				found = len(node.Args) > 1 && mentions(node.Args[1])
			case returnsOperation(callee) || set[callee]:
				found = slices.ContainsFunc(node.Args, mentions)
			}
		}
		return !found
	})
	return found
}

// stringParams returns the string-typed parameters of fn.
func stringParams(info *types.Info, fn *ast.FuncDecl) map[*types.Var]bool {
	params := map[*types.Var]bool{}
	for _, param := range paramObjects(info, fn) {
		if param == nil {
			continue
		}
		if basic, ok := param.Type().Underlying().(*types.Basic); ok && basic.Kind() == types.String {
			params[param] = true
		}
	}
	return params
}

// mentionsAny reports whether expr references any of the given variables.
func mentionsAny(info *types.Info, expr ast.Expr, vars map[*types.Var]bool) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		if found {
			return false
		}
		if ident, ok := n.(*ast.Ident); ok {
			if v, ok := info.Uses[ident].(*types.Var); ok && vars[v] {
				found = true
			}
		}
		return !found
	})
	return found
}

func returnsOperation(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok {
		return false
	}
	for res := range sig.Results().Variables() {
		if isHumaNamed(res.Type(), "Operation") {
			return true
		}
	}
	return false
}

// match reports the registered route that a hand-written URL literal hits,
// if any. The literal may carry a base URL, format verbs, template holes, a
// query string, concrete parameter values, or a trailing slash awaiting a
// concatenated identifier.
func (rs *routeSet) match(literal string) (string, bool) {
	if rs == nil || len(rs.concrete) == 0 {
		return "", false
	}
	path := extractPath(literal)
	if path == "" || path == "/" || strings.Contains(path, "//") {
		return "", false
	}
	open := strings.HasSuffix(path, "/")
	segments := strings.Split(strings.Trim(path, "/"), "/")
	// A path made only of placeholders ("/%s", "/{id}") would match every
	// single-segment route; require at least one literal segment.
	if !slices.ContainsFunc(segments, func(seg string) bool { return !wildSegment(seg) }) {
		return "", false
	}
	for _, route := range rs.concrete {
		if segmentsMatch(segments, open, strings.Split(strings.TrimPrefix(route, "/"), "/")) {
			return paramPattern.ReplaceAllString(route, "{param}"), true
		}
	}
	return "", false
}

var verbPattern = regexp.MustCompile(`%[-+# 0-9.]*[a-zA-Z]`)

// wildSegment reports whether a literal segment stands for a runtime value:
// a format verb, a template hole, or a {param}.
func wildSegment(seg string) bool {
	return verbPattern.MatchString(seg) || paramPattern.MatchString(seg)
}

// segmentsMatch compares a literal path to a route segment by segment. Route
// parameters and literal placeholders match any single segment; an open
// literal (trailing slash) must be a proper prefix of the route; a route
// ending in a {name...} wildcard absorbs the remaining literal segments.
func segmentsMatch(literal []string, open bool, route []string) bool {
	if len(route) == 0 {
		return false
	}
	last := route[len(route)-1]
	wildcardTail := strings.HasPrefix(last, "{") && strings.HasSuffix(last, "...}")
	switch {
	case open:
		if len(route) <= len(literal) {
			return false
		}
		route = route[:len(literal)]
	case wildcardTail:
		if len(literal) < len(route) {
			return false
		}
		literal = literal[:len(route)-1]
		route = route[:len(route)-1]
	case len(literal) != len(route):
		return false
	}
	for i := range route {
		if paramPattern.MatchString(route[i]) || wildSegment(literal[i]) {
			continue
		}
		if route[i] != literal[i] {
			return false
		}
	}
	return true
}

// extractPath pulls the URL path portion out of a string literal: the text
// from the first "/" after any scheme and host up to a query, fragment, or
// whitespace. Prose with spaces before the path is not a URL.
func extractPath(literal string) string {
	s := literal
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	i := strings.IndexByte(s, '/')
	if i < 0 || strings.ContainsAny(s[:i], " \t\n") {
		return ""
	}
	s = s[i:]
	if j := strings.IndexAny(s, "?# \t\n\"'`"); j >= 0 {
		s = s[:j]
	}
	return s
}
