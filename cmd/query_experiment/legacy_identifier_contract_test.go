package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQueryExperimentLegacyQueryRetrievalIdentifiersAreNotUsedInLiveCode(t *testing.T) {
	fset := token.NewFileSet()
	names := []string{}
	if _, err := os.Stat("three_host.go"); err == nil {
		names = append(names, "legacy file still present: three_host.go")
	}
	if err := filepath.Walk(".", func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if ok && (strings.Contains(ident.Name, "threeHost") || strings.Contains(ident.Name, "ThreeHost")) {
				names = append(names, path+":"+ident.Name)
			}
			return true
		})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 {
		return
	}
	t.Fatalf("found legacy query-retrieval identifiers: %v", names)
}
