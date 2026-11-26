package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestRunDrainsClaimedRefreshesBeforeItRetires holds the stop sequence: run must defer the
// refresh drain after the incarnation retire, so the drain runs first and a claimed refresh
// writes its rotated token back before Home stops.
func TestRunDrainsClaimedRefreshesBeforeItRetires(t *testing.T) {
	file, errParse := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if errParse != nil {
		t.Fatal(errParse)
	}
	calls := func(node ast.Node, pkg, name string) bool {
		found := false
		ast.Inspect(node, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != name {
				return true
			}
			if ident, ok := selector.X.(*ast.Ident); ok && (pkg == "" || ident.Name == pkg) {
				found = true
			}
			return true
		})
		return found
	}
	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "run" {
			run = fn
		}
	}
	if run == nil {
		t.Fatal("run not found")
	}
	retire, drain := token.NoPos, token.NoPos
	ast.Inspect(run.Body, func(n ast.Node) bool {
		deferred, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		if calls(deferred, "", "RetireHomeIncarnation") {
			retire = deferred.Pos()
		}
		if calls(deferred, "cluster", "DrainRefreshes") {
			drain = deferred.Pos()
		}
		return true
	})
	if drain == token.NoPos {
		t.Fatal("run does not defer cluster.DrainRefreshes")
	}
	if retire == token.NoPos || drain < retire {
		t.Fatal("the refresh drain must be deferred after the incarnation retire, so it runs first")
	}
}
