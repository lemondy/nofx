package trader

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// Opening one side must never erase another side's or a manual order's protection.
func TestVenueOpenNeverBulkCancels(t *testing.T) {
	for _, venue := range []string{"bybit", "okx", "gate", "hyperliquid"} {
		file, err := parser.ParseFile(token.NewFileSet(), venue+"/trader_orders.go", nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || (fn.Name.Name != "OpenLong" && fn.Name.Name != "OpenShort") {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if ok && (sel.Sel.Name == "CancelAllOrders" || sel.Sel.Name == "CancelStopOrders") {
					t.Errorf("%s.%s bulk cancels existing protection", venue, fn.Name.Name)
				}
				return true
			})
		}
	}
}
