// Package interfacecheck requires explicit interface assertions on types that
// implement a configured base interface and declare an optional interface method.
// It checks production and test files. Without configuration it reports nothing.
package interfacecheck

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/packages"
)

// Analyzer checks interfaces selected through its flags.
var Analyzer = New(nil, nil)

// New creates an analyzer. Bases are fully qualified interface names, such as
// example.org/project/platform.Provider. Packages are full import paths whose
// exported interfaces require assertions. Base interfaces themselves are excluded.
func New(bases, packages []string) *analysis.Analyzer {
	baseFlag, packageFlag := strings.Join(bases, ","), strings.Join(packages, ",")
	a := &analysis.Analyzer{
		Name: "interfacecheck",
		Doc:  "requires explicit interface assertions for types implementing configured base interfaces",
		Run: func(pass *analysis.Pass) (any, error) {
			// go/analysis does not supply a caller context for a pass.
			return run(context.Background(), pass, strings.Split(baseFlag, ","), strings.Split(packageFlag, ","))
		},
	}
	a.Flags.StringVar(&baseFlag, "base-interfaces", baseFlag, "comma-separated fully qualified base interface names")
	a.Flags.StringVar(&packageFlag, "interface-packages", packageFlag, "comma-separated import paths containing exported interfaces")
	return a
}

type contract struct {
	typ   types.Type
	iface *types.Interface
	name  string
}

func run(ctx context.Context, pass *analysis.Pass, bases, paths []string) (any, error) {
	if len(pass.Files) == 0 || len(bases) == 1 && bases[0] == "" {
		return nil, nil
	}
	// The type graph includes transitive imports, including a base inherited
	// through an embedded type from another package.
	loaded := map[string]*types.Package{}
	var visit func(*types.Package)
	visit = func(pkg *types.Package) {
		if loaded[pkg.Path()] != nil {
			return
		}
		loaded[pkg.Path()] = pkg
		for _, imported := range pkg.Imports() {
			visit(imported)
		}
	}
	visit(pass.Pkg)
	// Load configured packages even when a candidate does not import them.
	missing := map[string]bool{}
	for _, name := range bases {
		if path, _, ok := splitName(name); ok && loaded[path] == nil {
			missing[path] = true
		}
	}
	for _, path := range paths {
		if path != "" && loaded[path] == nil {
			missing[path] = true
		}
	}
	if len(missing) > 0 {
		patterns := make([]string, 0, len(missing))
		for path := range missing {
			patterns = append(patterns, path)
		}
		pkgs, err := packages.Load(&packages.Config{
			Context: ctx,
			Mode:    packages.NeedName | packages.NeedTypes | packages.NeedImports | packages.NeedDeps,
		}, patterns...)
		if err != nil {
			return nil, fmt.Errorf("interfacecheck: load configured packages: %w", err)
		}
		for _, pkg := range pkgs {
			if len(pkg.Errors) > 0 {
				return nil, fmt.Errorf("interfacecheck: load %s: %w", pkg.PkgPath, pkg.Errors[0])
			}
			if existing := loaded[pkg.PkgPath]; existing != nil && existing.Complete() {
				continue
			}
			// Reuse the pass's package identities, including shared named types in
			// method signatures, when importing the independently loaded contracts.
			var data bytes.Buffer
			if err := gcexportdata.Write(&data, pkg.Fset, pkg.Types); err != nil {
				return nil, fmt.Errorf("interfacecheck: export %s: %w", pkg.PkgPath, err)
			}
			if _, err := gcexportdata.Read(&data, pass.Fset, loaded, pkg.PkgPath); err != nil {
				return nil, fmt.Errorf("interfacecheck: import %s: %w", pkg.PkgPath, err)
			}
		}
	}
	var baseInterfaces []*types.Interface
	isBase := map[types.Type]bool{}
	for _, name := range bases {
		path, ident, ok := splitName(name)
		if !ok || loaded[path] == nil {
			continue
		}
		obj, ok := loaded[path].Scope().Lookup(ident).(*types.TypeName)
		if !ok {
			continue
		}
		if iface, ok := obj.Type().Underlying().(*types.Interface); ok {
			baseInterfaces = append(baseInterfaces, iface.Complete())
			isBase[types.Unalias(obj.Type())] = true
		}
	}
	var contracts []contract
	for _, path := range paths {
		pkg := loaded[path]
		if pkg == nil {
			continue
		}
		for _, name := range pkg.Scope().Names() {
			obj, ok := pkg.Scope().Lookup(name).(*types.TypeName)
			if !ok || !obj.Exported() || isBase[types.Unalias(obj.Type())] {
				continue
			}
			if iface, ok := obj.Type().Underlying().(*types.Interface); ok && iface.Complete().IsMethodSet() {
				contracts = append(contracts, contract{obj.Type(), iface.Complete(), pkg.Name() + "." + name})
			}
		}
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
			candidate := false
			for _, base := range baseInterfaces {
				if types.Implements(named, base) || types.Implements(types.NewPointer(named), base) {
					candidate = true
					break
				}
			}
			if !candidate {
				return true
			}
			for _, c := range contracts {
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
					if types.Identical(target, c.typ) || concrete(target) != nil && concrete(target) == concrete(c.typ) {
						asserted = true
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

func splitName(name string) (string, string, bool) {
	i := strings.LastIndexByte(name, '.')
	if i < 1 || i == len(name)-1 {
		return "", "", false
	}
	return name[:i], name[i+1:], true
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
