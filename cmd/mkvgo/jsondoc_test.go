package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestJSONFlagDocumented: docs/cli.md lists exactly the commands that print a
// JSON report - their Cmd function, or a function it calls, calls PrintJSON
// (errors go out as JSON for every command: not a report) - and its jq
// examples address snake_case keys, the only ones the JSON reports carry.
func TestJSONFlagDocumented(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, "commands", func(fi os.FileInfo) bool { return !strings.HasSuffix(fi.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	funcs := map[string]*ast.FuncDecl{}
	for _, f := range pkgs["commands"].Files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				funcs[fd.Name.Name] = fd
			}
		}
	}
	// readsJSON: the function calls PrintJSON, or calls a package function that does, three calls deep at most.
	var readsJSON func(name string, depth int) bool
	readsJSON = func(name string, depth int) bool {
		fd := funcs[name]
		if fd == nil || fd.Body == nil {
			return false
		}
		found := false
		ast.Inspect(fd.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok {
					found = found || id.Name == "PrintJSON" || (depth > 0 && id.Name != name && readsJSON(id.Name, depth-1))
				}
			}
			return !found
		})
		return found
	}
	main, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var withJSON []string
	ast.Inspect(main, func(n ast.Node) bool {
		cc, ok := n.(*ast.CaseClause)
		if !ok || len(cc.List) != 1 {
			return true
		}
		lit, ok := cc.List[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		cmd, _ := strconv.Unquote(lit.Value)
		for _, st := range cc.Body {
			ast.Inspect(st, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "Cmd") && readsJSON(sel.Sel.Name, 3) {
					if !slices.Contains(withJSON, cmd) {
						withJSON = append(withJSON, cmd)
					}
				}
				return true
			})
		}
		return true
	})
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "cli.md"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`the command's report as JSON on stdout \(([^)]*);`).FindSubmatch(doc)
	if m == nil {
		t.Fatal("docs/cli.md: the -json flag's command list is not where this test looks")
	}
	var listed []string
	for _, c := range strings.Split(string(m[1]), ",") {
		listed = append(listed, strings.TrimSpace(c))
	}
	slices.Sort(withJSON)
	slices.Sort(listed)
	for _, c := range withJSON {
		if !slices.Contains(listed, c) {
			t.Errorf("%s prints a JSON report but docs/cli.md does not list it", c)
		}
	}
	for _, c := range listed {
		if !slices.Contains(withJSON, c) {
			t.Errorf("docs/cli.md lists %s under -json, its code prints no JSON report", c)
		}
	}
	for _, line := range strings.Split(string(doc), "\n") {
		if !strings.Contains(line, "| jq") {
			continue
		}
		if k := regexp.MustCompile(`\.[A-Z][A-Za-z]*`).FindString(line[strings.Index(line, "| jq"):]); k != "" {
			t.Errorf("docs/cli.md: jq reads %s, the JSON keys are snake_case: %s", k, line)
		}
	}
}
