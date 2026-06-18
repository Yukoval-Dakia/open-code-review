// internal/impact/go_analyzer.go
package impact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
)

type goAnalyzer struct{}

func (goAnalyzer) Supports(path string) bool { return strings.HasSuffix(path, ".go") }

func (goAnalyzer) ChangedSymbols(content string, changed map[int]bool) ([]Symbol, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, 0)
	if err != nil {
		return nil, err
	}
	var out []Symbol
	add := func(name string, kind string, pos token.Pos) {
		line := fset.Position(pos).Line
		if changed[line] {
			out = append(out, Symbol{Name: name, Kind: kind, DefLine: line})
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			kind := "function"
			if d.Recv != nil {
				kind = "method"
			}
			add(d.Name.Name, kind, d.Name.Pos())
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					add(s.Name.Name, "type", s.Name.Pos())
				case *ast.ValueSpec:
					for _, n := range s.Names {
						add(n.Name, "const", n.Pos())
					}
				}
			}
		}
	}
	return out, nil
}

func (goAnalyzer) References(path, content, name string) ([]Reference, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", content, 0)
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var refs []Reference
	ast.Inspect(f, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || id.Name != name {
			return true
		}
		// Skip the definition site itself and duplicate lines.
		line := fset.Position(id.Pos()).Line
		if seen[line] {
			return true
		}
		seen[line] = true
		refs = append(refs, Reference{File: path, Line: line, Kind: "ref"})
		return true
	})
	return refs, nil
}
