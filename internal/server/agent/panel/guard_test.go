package panel

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPanelPackageMustUseCommLayerWithoutDirectHTTPOrSigning(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		file, parseErr := parser.ParseFile(fset, filepath.Clean(name), nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}

		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				ident, ok := node.X.(*ast.Ident)
				if !ok {
					return true
				}
				if ident.Name == "http" {
					switch node.Sel.Name {
					case "NewRequest", "NewRequestWithContext", "Get", "Post", "Head", "Client", "Transport", "DefaultClient", "DefaultTransport":
						t.Errorf("%s references http.%s directly; all panel HTTP calls must go through comm.Client", name, node.Sel.Name)
					}
				}
				if ident.Name == "protocol" && (node.Sel.Name == "Signature" || node.Sel.Name == "SigningPayload") {
					t.Errorf("%s signs requests via protocol.%s directly; signing must stay inside comm", name, node.Sel.Name)
				}
			}
			return true
		})
	}
}
