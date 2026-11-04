// Package interfacecheck requires explicit interface assertions on types that
// implement a marked base interface and declare an optional interface method.
// It checks production and test files.
package interfacecheck

import (
	"go/ast"
	"go/token"
	"go/types"

	"golang.org/x/tools/go/analysis"
)

// Analyzer checks interfaces in packages declaring a //kit:interfacecheck base.
var Analyzer = &analysis.Analyzer{
	Name:      "interfacecheck",
	Doc:       "requires explicit interface assertions for types implementing marked base interfaces",
	Run:       run,
	FactTypes: []analysis.Fact{new(baseFact)},
}

type baseFact struct{}

func (*baseFact) AFact() {}

type contract struct {
	typ   types.Type
	iface *types.Interface
	name  string
	base  *types.Interface
}

func run(pass *analysis.Pass) (any, error) {
	marked := map[*types.TypeName]bool{}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				spec := spec.(*ast.TypeSpec)
				doc := spec.Doc
				if doc == nil && len(gen.Specs) == 1 {
					doc = gen.Doc
				}
				if doc == nil {
					continue
				}
				for _, comment := range doc.List {
					if comment.Text != "//kit:interfacecheck" {
						continue
					}
					obj := pass.TypesInfo.Defs[spec.Name].(*types.TypeName)
					if interfaceType(obj) != nil {
						marked[obj] = true
						pass.ExportObjectFact(obj, new(baseFact))
					}
				}
			}
		}
	}
	var contracts []contract
	visited := map[*types.Package]bool{}
	var visit func(*types.Package)
	visit = func(pkg *types.Package) {
		if visited[pkg] {
			return
		}
		visited[pkg] = true
		for _, name := range pkg.Scope().Names() {
			obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok {
				continue
			}
			base := interfaceType(obj)
			if base == nil || !marked[obj] && (pkg == pass.Pkg || !pass.ImportObjectFact(obj, new(baseFact))) {
				continue
			}
			for _, name := range pkg.Scope().Names() {
				optional, ok := pkg.Scope().Lookup(name).(*types.TypeName)
				if !ok || !optional.Exported() || optional == obj {
					continue
				}
				if iface := interfaceType(optional); iface != nil {
					contracts = append(contracts, contract{optional.Type(), iface, pkg.Name() + "." + name, base})
				}
			}
		}
		for _, imported := range pkg.Imports() {
			visit(imported)
		}
	}
	// A package that does not transitively import the marked package cannot
	// satisfy its base: the base methods use types from that package.
	visit(pass.Pkg)
	if len(contracts) == 0 {
		return nil, nil
	}
	assertions := map[*types.Named][]types.Type{}
	for _, file := range pass.Files {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.VAR {
				continue
			}
			for _, spec := range gen.Specs {
				value := spec.(*ast.ValueSpec)
				var sources []types.Type
				conversions := map[int][]types.Type{}
				for _, expr := range value.Values {
					if tuple, ok := pass.TypesInfo.TypeOf(expr).(*types.Tuple); ok {
						for variable := range tuple.Variables() {
							sources = append(sources, variable.Type())
						}
						continue
					}
					// Interface conversions also ask the compiler to check
					// the concrete operand, even with an inferred var type.
					for {
						call, ok := ast.Unparen(expr).(*ast.CallExpr)
						if !ok || len(call.Args) != 1 || !pass.TypesInfo.Types[call.Fun].IsType() {
							break
						}
						if _, ok := pass.TypesInfo.TypeOf(call.Fun).Underlying().(*types.Interface); !ok {
							break
						}
						conversions[len(sources)] = append(conversions[len(sources)], pass.TypesInfo.TypeOf(call.Fun))
						expr = call.Args[0]
					}
					sources = append(sources, pass.TypesInfo.TypeOf(expr))
				}
				for i, name := range value.Names {
					if name.Name != "_" || i >= len(sources) {
						continue
					}
					target := pass.TypesInfo.Defs[name].(*types.Var).Type()
					if _, ok := target.Underlying().(*types.Interface); !ok {
						continue
					}
					if named := concrete(sources[i]); named != nil {
						assertions[named] = append(assertions[named], target)
						assertions[named] = append(assertions[named], conversions[i]...)
					}
				}
			}
		}
	}
	for _, file := range pass.Files {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok {
				return true
			}
			obj := pass.TypesInfo.Defs[spec.Name].(*types.TypeName)
			named, ok := obj.Type().(*types.Named)
			if !ok {
				return true
			}
			if _, ok := named.Underlying().(*types.Interface); ok {
				return true
			}
			for _, c := range contracts {
				if !types.Implements(named, c.base) && !types.Implements(types.NewPointer(named), c.base) {
					continue
				}
				shared := false
				for method := range named.Methods() {
					for explicit := range c.iface.ExplicitMethods() {
						if method.Id() == explicit.Id() {
							shared = true
						}
					}
				}
				if !shared {
					continue
				}
				asserted := false
				for _, target := range assertions[named.Origin()] {
					if types.Identical(target, c.typ) {
						asserted = true
					}
					if asserted {
						break
					}
				}
				if !asserted {
					pass.Reportf(spec.Name.Pos(), "type %s must declare a package-level interface assertion for %s", spec.Name.Name, c.name)
				}
			}
			return true
		})
	}
	return nil, nil
}

// Generic interfaces and aliases are outside the assertion rule.
func interfaceType(obj *types.TypeName) *types.Interface {
	if obj.IsAlias() {
		return nil
	}
	named, ok := obj.Type().(*types.Named)
	if !ok || named.TypeParams().Len() != 0 {
		return nil
	}
	iface, ok := named.Underlying().(*types.Interface)
	if !ok || !iface.Complete().IsMethodSet() {
		return nil
	}
	return iface
}

func concrete(typ types.Type) *types.Named {
	typ = types.Unalias(typ)
	if pointer, ok := typ.(*types.Pointer); ok {
		typ = types.Unalias(pointer.Elem())
	}
	named, _ := typ.(*types.Named)
	if named != nil {
		return named.Origin()
	}
	return nil
}
