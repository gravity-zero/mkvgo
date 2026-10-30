package oneopt

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestEveryExportedFunctionChecksItsOptions: an exported function taking variadic options read only the first and
// ignored the others without a word. Each one, in every public package, opens on an if on oneopt.Check of its options;
// a constructor that returns no error hands back a value that fails with the refusal, never a panic.
func TestEveryExportedFunctionChecksItsOptions(t *testing.T) {
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	seen := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			switch strings.Split(filepath.ToSlash(rel), "/")[0] {
			case "cmd", "internal", "examples", "bench", "web", "scripts", "dist":
				return filepath.SkipDir
			}
			if d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".") && rel != "." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() || fn.Body == nil || !exportedRecv(fn) {
				continue
			}
			name, ok := variadicOptions(fn.Type)
			if !ok {
				continue
			}
			seen++
			if !opensOnCheck(fn.Body, name) {
				t.Errorf("%s: %s takes ...Options but does not open on oneopt.Check(%s)", fset.Position(fn.Pos()), fn.Name.Name, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 100 {
		t.Fatalf("%d exported functions with variadic options found: the walk missed the packages", seen)
	}
}

func exportedRecv(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return true
	}
	x := fn.Recv.List[0].Type
	if s, ok := x.(*ast.StarExpr); ok {
		x = s.X
	}
	if ix, ok := x.(*ast.IndexExpr); ok {
		x = ix.X
	}
	id, ok := x.(*ast.Ident)
	return ok && id.IsExported()
}

// variadicOptions returns the name of fn's last parameter when it is ...Options (any package's).
func variadicOptions(ft *ast.FuncType) (string, bool) {
	ps := ft.Params.List
	if len(ps) == 0 {
		return "", false
	}
	last := ps[len(ps)-1]
	el, ok := last.Type.(*ast.Ellipsis)
	if !ok || len(last.Names) != 1 {
		return "", false
	}
	switch x := el.Elt.(type) {
	case *ast.Ident:
		ok = x.Name == "Options"
	case *ast.SelectorExpr:
		ok = x.Sel.Name == "Options"
	default:
		ok = false
	}
	return last.Names[0].Name, ok
}

// opensOnCheck reports whether body's first statement is an if on oneopt.Check(name).
func opensOnCheck(body *ast.BlockStmt, name string) bool {
	if len(body.List) == 0 {
		return false
	}
	var call ast.Expr
	if s, ok := body.List[0].(*ast.IfStmt); ok {
		if as, ok := s.Init.(*ast.AssignStmt); ok && len(as.Rhs) == 1 {
			call = as.Rhs[0]
		}
	}
	c, ok := call.(*ast.CallExpr)
	if !ok || len(c.Args) != 1 {
		return false
	}
	sel, ok := c.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	arg, ok2 := c.Args[0].(*ast.Ident)
	return ok && ok2 && pkg.Name == "oneopt" && sel.Sel.Name == "Check" && arg.Name == name
}
