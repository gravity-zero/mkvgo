package reader

import (
	"go/ast"
	goparser "go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// TestMatroskaIDsListsEveryID: every ID constant of package mkv is in matroskaIDs.
func TestMatroskaIDsListsEveryID(t *testing.T) {
	f, err := goparser.ParseFile(token.NewFileSet(), filepath.Join("..", "ids.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			continue
		}
		for _, s := range g.Specs {
			for _, name := range s.(*ast.ValueSpec).Names {
				if strings.HasPrefix(name.Name, "ID") {
					n++
				}
			}
		}
	}
	if n != len(matroskaIDs) {
		t.Errorf("package mkv declares %d IDs, matroskaIDs holds %d", n, len(matroskaIDs))
	}
}
