package mkvgo

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestExportedSymbolsDocumented: every exported symbol of the public packages -
// function, method of an exported type, type, constant, variable - carries a
// doc comment: its own, the one of the const/var/type group it sits in, or a
// comment at the end of its line in such a group. internal, cmd, examples and
// bench are not API.
func TestExportedSymbolsDocumented(t *testing.T) {
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch name := d.Name(); {
			case path == ".":
			case name == "internal", name == "cmd", name == "examples", name == "bench", name == "testdata",
				strings.HasPrefix(name, "."), strings.HasPrefix(name, "_"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		if f.Name.Name == "main" {
			return nil
		}
		missing := func(pos token.Pos, name string) {
			t.Errorf("%s: exported %s has no doc comment", fset.Position(pos), name)
		}
		for _, decl := range f.Decls {
			switch d := decl.(type) {
			case *ast.FuncDecl:
				if d.Name.IsExported() && receiverExported(d) && d.Doc == nil {
					missing(d.Pos(), d.Name.Name)
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					var names []*ast.Ident
					var doc, line *ast.CommentGroup
					switch s := spec.(type) {
					case *ast.TypeSpec:
						names, doc, line = []*ast.Ident{s.Name}, s.Doc, s.Comment
					case *ast.ValueSpec:
						names, doc, line = s.Names, s.Doc, s.Comment
					}
					for _, n := range names {
						if n.IsExported() && doc == nil && d.Doc == nil && (line == nil || !d.Lparen.IsValid()) {
							missing(n.Pos(), n.Name)
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
}

// receiverExported reports whether a function is a plain function or a method of an exported type.
func receiverExported(d *ast.FuncDecl) bool {
	if d.Recv == nil {
		return true
	}
	t := d.Recv.List[0].Type
	if s, ok := t.(*ast.StarExpr); ok {
		t = s.X
	}
	switch x := t.(type) {
	case *ast.IndexExpr:
		t = x.X
	case *ast.IndexListExpr:
		t = x.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.IsExported()
}
