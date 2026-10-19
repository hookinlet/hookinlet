// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inboxtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// notStoreCapabilities are the interfaces in pkg/inbox that are not optional
// capabilities of a Store. With optionalStoreInterfaces it accounts for every
// interface the package declares, so that a new one is a decision: if it is
// something the server finds on a Store by type assertion, it goes in
// optionalStoreInterfaces, and AssertStoreWrapperForwards then checks it.
var notStoreCapabilities = []string{
	"Store", "Notifier", "Authenticator", "Principal", "ScopedPrincipal", "TenantPrincipal",
	"Hooks", "Admitter", "UnverifiedLimiter", "IngestObserver", "BodyScreener", "SecretBox", "ContextSecretBox", "RetryAfterer", "Challenger", "Unrouted",
}

func optionalStoreNames() []string {
	var names []string
	for _, o := range optionalStoreInterfaces {
		names = append(names, o.typ.Name())
	}
	return names
}

// Every interface pkg/inbox declares is either an optional capability of a Store,
// and in optionalStoreInterfaces, or listed as not one.
func TestEveryInboxInterfaceIsClassified(t *testing.T) {
	optional := optionalStoreNames()
	fset := token.NewFileSet()
	dir := filepath.Join("..", "inbox")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts := spec.(*ast.TypeSpec)
				if _, ok := ts.Type.(*ast.InterfaceType); !ok || !ts.Name.IsExported() {
					continue
				}
				name := ts.Name.Name
				seen[name] = true
				doc := ts.Doc
				if doc == nil {
					doc = gd.Doc
				}
				isOptional, isOther := slices.Contains(optional, name), slices.Contains(notStoreCapabilities, name)
				switch {
				case isOptional && isOther:
					t.Errorf("inbox.%s is in both lists", name)
				case !isOptional && !isOther:
					t.Errorf("inbox.%s is new: if the server finds it on a Store by type assertion, add it to optionalStoreInterfaces "+
						"(wrapper.go) and to docs/extending.md; otherwise add it to notStoreCapabilities", name)
				case isOther && strings.Contains(doc.Text(), "capability of a Store"):
					t.Errorf("inbox.%s says it is an optional capability of a Store, but is listed in notStoreCapabilities", name)
				}
			}
		}
	}
	for _, name := range append(optional, notStoreCapabilities...) {
		if !seen[name] {
			t.Errorf("inbox.%s is listed but pkg/inbox no longer declares it", name)
		}
	}
}

// Every interface the module's code asserts a Store to (s.Store.(inbox.X),
// opts.Store.(inbox.X), ...) is in optionalStoreInterfaces. This catches an optional
// capability the server uses that was left out, wherever it is declared.
func TestEveryStoreTypeAssertionIsListed(t *testing.T) {
	optional := optionalStoreNames()
	root := filepath.Join("..", "..")
	fset := token.NewFileSet()
	found := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); path != root && (strings.HasPrefix(n, ".") || n == "node_modules" || n == "testdata" || n == "web") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			ta, ok := n.(*ast.TypeAssertExpr)
			if !ok || ta.Type == nil || !assertsOnAStore(ta.X) {
				return true
			}
			sel, ok := ta.Type.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			found++
			if !slices.Contains(optional, sel.Sel.Name) {
				t.Errorf("%s asserts a Store to %s, which is not in optionalStoreInterfaces (wrapper.go); add it, and say in docs/extending.md what the server does without it",
					fset.Position(ta.Pos()), sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no type assertions on a Store; the scan is broken")
	}
}

// assertsOnAStore reports whether x names a Store: a field or variable called
// Store, or a selector ending in .Store.
func assertsOnAStore(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name == "Store"
	case *ast.Ident:
		return v.Name == "Store" || v.Name == "store"
	}
	return false
}
