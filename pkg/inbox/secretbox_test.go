// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (C) 2026 Pat Ramsey

package inbox_test

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/hookinlet/hookinlet/pkg/inbox"
)

// plainBox is a SecretBox without the context methods. It "seals" by prefixing.
type plainBox struct{ seals, opens int }

func (b *plainBox) Seal(pt, binding []byte) ([]byte, error) {
	b.seals++
	return append(append([]byte("p:"), binding...), pt...), nil
}

func (b *plainBox) Open(ct, binding []byte) ([]byte, error) {
	b.opens++
	return []byte(strings.TrimPrefix(string(ct), "p:"+string(binding))), nil
}

// ctxBox is a SecretBox with the context methods, which it counts apart.
type ctxBox struct {
	plainBox
	ctxSeals, ctxOpens int
	seen               context.Context
}

func (b *ctxBox) SealContext(ctx context.Context, pt, binding []byte) ([]byte, error) {
	b.ctxSeals++
	b.seen = ctx
	return append(append([]byte("c:"), binding...), pt...), nil
}

func (b *ctxBox) OpenContext(ctx context.Context, ct, binding []byte) ([]byte, error) {
	b.ctxOpens++
	b.seen = ctx
	return []byte(strings.TrimPrefix(string(ct), "c:"+string(binding))), nil
}

var _ inbox.ContextSecretBox = (*ctxBox)(nil)

type ctxKey struct{}

// The helpers give the context to a box that takes one, and never call its plain
// methods.
func TestTheSecretHelpersPreferTheContextMethods(t *testing.T) {
	b := &ctxBox{}
	ctx := context.WithValue(t.Context(), ctxKey{}, "mine")
	ct, err := inbox.SealWith(ctx, b, []byte("pt"), []byte("bind"))
	if err != nil || string(ct) != "c:bindpt" {
		t.Fatalf("SealWith: %q, %v", ct, err)
	}
	if b.seen == nil || b.seen.Value(ctxKey{}) != "mine" {
		t.Error("SealContext was not given the caller's context")
	}
	b.seen = nil
	pt, err := inbox.OpenWith(ctx, b, ct, []byte("bind"))
	if err != nil || string(pt) != "pt" {
		t.Fatalf("OpenWith: %q, %v", pt, err)
	}
	if b.seen == nil || b.seen.Value(ctxKey{}) != "mine" {
		t.Error("OpenContext was not given the caller's context")
	}
	if b.ctxSeals != 1 || b.ctxOpens != 1 || b.seals != 0 || b.opens != 0 {
		t.Errorf("context seals %d opens %d, plain seals %d opens %d; want 1 1 0 0", b.ctxSeals, b.ctxOpens, b.seals, b.opens)
	}
}

// A box without them is called as before.
func TestTheSecretHelpersFallBackToThePlainMethods(t *testing.T) {
	b := &plainBox{}
	ct, err := inbox.SealWith(t.Context(), b, []byte("pt"), []byte("bind"))
	if err != nil || string(ct) != "p:bindpt" {
		t.Fatalf("SealWith: %q, %v", ct, err)
	}
	pt, err := inbox.OpenWith(t.Context(), b, ct, []byte("bind"))
	if err != nil || string(pt) != "pt" {
		t.Fatalf("OpenWith: %q, %v", pt, err)
	}
	if b.seals != 1 || b.opens != 1 {
		t.Errorf("plain seals %d opens %d, want 1 1", b.seals, b.opens)
	}
}

// secretBoxCallExempt are the non-test files allowed to call a SecretBox's Seal or
// Open directly: the helpers themselves, the conformance suite, which tests those
// methods, and the shipped box, whose context methods call its own.
var secretBoxCallExempt = map[string]bool{
	"pkg/inbox/interfaces.go":          true,
	"pkg/inboxtest/secretbox_suite.go": true,
	"pkg/secretbox/secretbox.go":       true,
}

// No code in the module seals or opens a secret except through SealWith and
// OpenWith, so none can forget to pass the context to a box that takes one. The scan
// is by syntax: a call X.Seal(a, b) or X.Open(a, b), with two arguments, where X is
// not a package (sqlite.Open(ctx, dir) and sql.Open(driver, dsn) are packages), is
// a SecretBox call. cipher.AEAD's Seal and Open take four.
func TestEverySecretIsSealedAndOpenedThroughTheHelpers(t *testing.T) {
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
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		pkgs := map[string]bool{}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			name := p[strings.LastIndex(p, "/")+1:]
			if imp.Name != nil {
				name = imp.Name.Name
			}
			pkgs[name] = true
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) != 2 {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Seal" && sel.Sel.Name != "Open") {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && pkgs[id.Name] {
				return true
			}
			found++
			if !secretBoxCallExempt[rel] {
				t.Errorf("%s calls %s directly: use inbox.%sWith(ctx, box, ...), so a SecretBox that takes a context gets one",
					fset.Position(call.Pos()), sel.Sel.Name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found == 0 {
		t.Fatal("found no SecretBox calls at all, not even in the helpers; the scan is broken")
	}
}
