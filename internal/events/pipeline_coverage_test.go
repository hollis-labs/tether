package events

import (
	"github.com/hollis-labs/libs/harness/core/adapters/runtimeevents/catalog"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// This new source guard complements runtime fixtures: it checks Tether kind
// constants, Event.Kind expressions and publishSessionEvent literals. Dynamic
// third-party/runtime kinds are C by absence, not automatically admitted. The
// reviewer must assess this guard's coverage, not treat it as a payload schema.
func TestEmittedSourceKindsHaveAdmissionPolicies(t *testing.T) {
	type file struct {
		node *ast.File
		path string
	}
	var files []file
	constants := map[string]ast.Expr{}
	fset := token.NewFileSet()
	err := filepath.WalkDir("..", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "pipeline_registry.go") {
			return nil
		}
		node, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		files = append(files, file{node, path})
		for _, decl := range node.Decls {
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				for _, spec := range gen.Specs {
					v := spec.(*ast.ValueSpec)
					for i, name := range v.Names {
						if i < len(v.Values) {
							constants[node.Name.Name+"."+name.Name] = v.Values[i]
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := catalog.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry.Kinds {
		name := "Kind"
		for _, part := range strings.FieldsFunc(entry.Kind, func(r rune) bool { return r == '.' || r == '_' || r == '-' }) {
			name += strings.ToUpper(part[:1]) + part[1:]
		}
		constants["catalog."+name] = &ast.BasicLit{Kind: token.STRING, Value: strconv.Quote(entry.Kind)}
	}
	var resolve func(ast.Expr, string, int) string
	resolve = func(expr ast.Expr, pkg string, depth int) string {
		if depth > 8 {
			return ""
		}
		switch e := expr.(type) {
		case *ast.BasicLit:
			if e.Kind == token.STRING {
				v, _ := strconv.Unquote(e.Value)
				return v
			}
		case *ast.Ident:
			return resolve(constants[pkg+"."+e.Name], pkg, depth+1)
		case *ast.SelectorExpr:
			if prefix, ok := e.X.(*ast.Ident); ok {
				return resolve(constants[prefix.Name+"."+e.Sel.Name], prefix.Name, depth+1)
			}
		}
		return ""
	}
	policies := pipelinePolicies()
	check := func(kind, path string) {
		if kind != "" {
			if _, ok := policies[kind]; !ok {
				t.Errorf("emitted kind %q lacks explicit admission policy (%s)", kind, path)
			}
		}
	}
	for key, expr := range constants {
		if strings.HasPrefix(key, "events.Kind") || strings.HasPrefix(key, "events.EventType") {
			check(resolve(expr, "events", 0), key)
		}
	}
	for _, f := range files {
		ast.Inspect(f.node, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.SelectorExpr:
				if prefix, ok := n.X.(*ast.Ident); ok && prefix.Name == "catalog" && strings.HasPrefix(n.Sel.Name, "Kind") {
					check(resolve(n, f.node.Name.Name, 0), f.path)
				}
			case *ast.CompositeLit:
				isEvent := false
				switch typ := n.Type.(type) {
				case *ast.Ident:
					isEvent = typ.Name == "Event"
				case *ast.SelectorExpr:
					isEvent = typ.Sel.Name == "Event"
				}
				if isEvent {
					for _, elt := range n.Elts {
						if kv, ok := elt.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Kind" {
								check(resolve(kv.Value, f.node.Name.Name, 0), f.path)
							}
						}
					}
				}
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && fn.Name == "publishSessionEvent" && len(n.Args) > 3 {
					check(resolve(n.Args[3], f.node.Name.Name, 0), f.path)
				}
			}
			return true
		})
	}
}
